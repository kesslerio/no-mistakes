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

func TestFreshSourcePublicationDoesNotRestampReboundPR(t *testing.T) {
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	dir, base, prior := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", remote)
	gitCmd(t, dir, "push", "origin", "main", "feature:destination")
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "fresh source work")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = remote
	sctx.Run.Branch = "feature"
	sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub}
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, head)
	body := filepath.Join(t.TempDir(), "body.md")
	original := compliantPipelineBody(t, prior)
	if err := os.WriteFile(body, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_PR_LIST_JSON=[]", "FAKE_CLI_PR_BODY_FILE="+body)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(body)
	if err != nil || string(after) != original {
		t.Fatal("fresh source publication restamped rebound PR")
	}
	if gitCmd(t, remote, "rev-parse", "refs/heads/destination") != prior {
		t.Fatal("destination head changed")
	}
	if gitCmd(t, remote, "rev-parse", "refs/heads/feature") != head {
		t.Fatal("fresh source was not published")
	}
}

func TestReboundPublicationUpdatesOnlyExistingDestination(t *testing.T) {
	for _, scenario := range []string{"fast-forward", "missing", "diverged", "replacement", "wrong-repository", "wrong-branch", "wrong-sha"} {
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
			identityURL, identityBranch, identitySHA := remote, "destination", prior
			switch scenario {
			case "wrong-repository":
				identityURL = remote + "-fork"
			case "wrong-branch":
				identityBranch = "other"
			case "wrong-sha":
				identitySHA = base
			}
			sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_IDENTITY_JSON="+fmt.Sprintf(`{"head":{"repo":{"clone_url":%q},"ref":%q,"sha":%q}}`, identityURL, identityBranch, identitySHA))
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

func TestReboundPRBaseFollowsLiveBaseUnlessExplicit(t *testing.T) {
	for _, scenario := range []struct{ live, override, expected string }{
		{"main", "", "main"},
		{"release", "", "release"},
		{"main", "release", "release"},
	} {
		t.Run(scenario.live+"/"+scenario.override, func(t *testing.T) {
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", remote)
			gitCmd(t, dir, "push", "origin", "main", "feature:destination", "main:release")
			gitCmd(t, dir, "branch", "release", base)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			sctx.Repo.UpstreamURL = remote
			sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub}
			url := "https://github.com/" + github.RepoSlug(remote) + "/pull/42"
			sctx.Run.PRURL = &url
			sctx.Run.Publication = &db.PublicationBinding{Branch: "destination", PRURL: url, Target: branchsync.TargetFingerprint(remote), BaseBranch: "main", SourceRun: "source"}
			if scenario.override != "" {
				sctx.Run.PRBaseBranch = &scenario.override
			}
			body := filepath.Join(t.TempDir(), "body.md")
			log := filepath.Join(t.TempDir(), "gh.log")
			if err := os.WriteFile(body, []byte(compliantPipelineBody(t, head)), 0o644); err != nil {
				t.Fatal(err)
			}
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`),
				"FAKE_CLI_PR_LIST_JSON="+fmt.Sprintf(`[{"number":42,"url":%q,"baseRefName":%q}]`, url, scenario.live),
				"FAKE_CLI_PR_BASE="+scenario.live, "FAKE_CLI_PR_BODY_FILE="+body, "FAKE_CLI_LOG="+log,
				"FAKE_CLI_PR_HEAD_IDENTITY_JSON="+fmt.Sprintf(`{"head":{"repo":{"clone_url":%q},"ref":"destination","sha":%q}}`, remote, head))
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.override == "" && strings.Contains(string(data), "--base ") {
				t.Fatalf("observed base became retarget: %s", data)
			}
			if scenario.override != "" && !strings.Contains(string(data), "--base "+scenario.expected) {
				t.Fatalf("explicit base not applied: %s", data)
			}
		})
	}
}
