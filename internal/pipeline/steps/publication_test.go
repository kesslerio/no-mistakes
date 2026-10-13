package steps

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReboundPublicationUpdatesOnlyExistingDestination(t *testing.T) {
	for _, scenario := range []string{"fast-forward", "missing", "diverged", "replacement"} {
		t.Run(scenario, func(t *testing.T) {
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			dir, base, prior := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", remote)
			gitCmd(t, dir, "push", "origin", "main", "feature:destination")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "successor validated work")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			if scenario == "missing" {
				gitCmd(t, remote, "update-ref", "-d", "refs/heads/destination")
			}
			if scenario == "diverged" {
				gitCmd(t, dir, "checkout", "--detach", prior)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "independent")
				gitCmd(t, dir, "push", "origin", "HEAD:destination")
				gitCmd(t, dir, "checkout", "--detach", head)
			}
			before := gitCmd(t, remote, "show-ref")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			sctx.Repo.UpstreamURL = remote
			sctx.Run.Branch = "feature"
			url := "https://github.com/" + github.RepoSlug(remote) + "/pull/42"
			sctx.Run.PRURL = &url
			sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub}
			sctx.Run.Publication = &db.PublicationBinding{Branch: "destination", PRURL: url, Target: branchsync.TargetFingerprint(sctx.Repo.UpstreamURL), SourceRun: "previous"}
			setupGateMirror(t, sctx)
			recordReviewApproval(t, sctx, head)
			body := filepath.Join(t.TempDir(), "body.md")
			if err := os.WriteFile(body, []byte(compliantPipelineBody(t, prior)), 0o644); err != nil {
				t.Fatal(err)
			}
			list := fmt.Sprintf(`[{"number":42,"url":%q,"baseRefName":"main"}]`, url)
			if scenario == "replacement" {
				list = fmt.Sprintf(`[{"number":43,"url":%q,"baseRefName":"main"}]`, strings.TrimSuffix(url, "42")+"43")
			}
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_PR_LIST_JSON="+list, "FAKE_CLI_PR_BODY_FILE="+body, "FAKE_CLI_PR_TITLE=fix: existing")
			_, err := (&PushStep{}).Execute(sctx)
			if scenario != "fast-forward" {
				if err == nil {
					t.Fatal("unsafe publication accepted")
				}
				if gitCmd(t, remote, "show-ref") != before {
					t.Fatal("refusal changed remote")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gitCmd(t, remote, "rev-parse", "refs/heads/destination") != head {
				t.Fatal("destination not advanced")
			}
			if refs := gitCmd(t, remote, "show-ref"); strings.Contains(refs, "refs/heads/feature") {
				t.Fatal("created replacement source branch")
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.PushRef == nil || *run.PushRef != "refs/heads/destination" {
				t.Fatalf("wrong push binding: %#v", run)
			}
			if gitCmd(t, sctx.GateDir, "rev-parse", "refs/heads/feature") != head {
				t.Fatal("source mirror not settled")
			}
		})
	}
}
