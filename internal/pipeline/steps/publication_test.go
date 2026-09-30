package steps

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

func TestReboundPublicationPushesOnlyAppendOnlyToExistingBranch(t *testing.T) {
	for _, scenario := range []string{"append", "advanced-managed", "rewrite", "missing", "advanced", "deleted-during-push", "closed-pr", "changed-target"} {
		t.Run(scenario, func(t *testing.T) {
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			dir, base, submitted := setupGitRepo(t)
			gitCmd(t, dir, "push", remote, "refs/heads/main:refs/heads/main")
			gitCmd(t, dir, "push", remote, "HEAD:refs/heads/existing")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "validated descendant")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "remote", "add", "origin", remote)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			repo, err := sctx.DB.UpdateRepoMetadataWithFork(sctx.Repo.ID, "https://github.com/test/repo", remote, "main")
			if err != nil {
				t.Fatal(err)
			}
			sctx.Repo = repo
			if err := sctx.DB.RebindPublication(repo, sctx.Run, "existing", "https://github.com/test/repo/pull/1", branchsync.TargetFingerprint(repo.PushURL())); err != nil {
				t.Fatal(err)
			}
			// The executor still holds its original custody-only Run. Publication
			// must read the destination from the database at the actual boundary.
			if sctx.Run.PublicationBranch != nil {
				t.Fatal("fixture accidentally bypasses durable reload")
			}
			sctx.Env, _ = fakeGH(t, "https://github.com/test/repo/pull/1")
			sctx.Env = append(sctx.Env, fmt.Sprintf(`FAKE_CLI_PR_LIST_JSON=[{"number":1,"url":"https://github.com/test/repo/pull/1","headRefName":"existing","headRepositoryOwner":{"login":%q}}]`, strings.Split(github.RepoSlug(remote), "/")[0]))
			sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+submitted)
			recordReviewApproval(t, sctx, head)
			setupGateMirror(t, sctx)
			before := submitted
			switch scenario {
			case "advanced-managed":
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "later reviewed correction")
				head = gitCmd(t, dir, "rev-parse", "HEAD")
				recordReviewApproval(t, sctx, head)
			case "deleted-during-push":
				path, _ := envValue(sctx.Env, "PATH")
				linkTestBinary(t, filepath.SplitList(path)[0], "git")
				sctx.Env = append(sctx.Env, "FAKE_CLI_MODE=gh-with-intervening-push", "FAKE_CLI_STATE=OPEN", "FAKE_CLI_REAL_GIT="+testGitExecutable, "FAKE_CLI_INTERLOPER_DIR="+dir, "FAKE_CLI_INTERLOPER_REMOTE="+remote, "FAKE_CLI_INTERLOPER_REF=:refs/heads/existing")
			case "closed-pr":
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_STATE=CLOSED")
			case "changed-target":
				if _, err := sctx.DB.UpdateRepoMetadataWithFork(repo.ID, repo.UpstreamURL, "https://github.com/other/repo", "main"); err != nil {
					t.Fatal(err)
				}
			case "rewrite":
				gitCmd(t, dir, "reset", "--hard", base)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "divergent history")
				head = gitCmd(t, dir, "rev-parse", "HEAD")
				sctx.Run.HeadSHA = head
				recordReviewApproval(t, sctx, head)
			case "missing":
				gitCmd(t, remote, "update-ref", "-d", "refs/heads/existing")
			case "advanced":
				gitCmd(t, dir, "checkout", "-b", "other", submitted)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "outside publisher")
				before = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "push", remote, "HEAD:refs/heads/existing")
				gitCmd(t, dir, "checkout", "feature")
			}
			_, err = (&PushStep{}).Execute(sctx)
			if scenario == "append" || scenario == "advanced-managed" {
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, remote, "rev-parse", "refs/heads/existing"); got != head {
					t.Fatalf("wrong published head: %s", got)
				}
				if sctx.Run.HeadSHA != head || sctx.Run.Branch != "refs/heads/feature" {
					t.Fatalf("executor state did not follow publication: %+v", sctx.Run)
				}
				if published, err := publishedBranchHead(sctx); err != nil || published != head {
					t.Fatalf("CI monitored the wrong branch: %s %v", published, err)
				}
				r, _ := sctx.DB.GetRun(sctx.Run.ID)
				if scenario == "append" {
					sctx.Env = append(sctx.Env, "FAKE_CLI_PR_LIST_JSON=[]")
					if _, err := (&PRStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "replacement PR") {
						t.Fatalf("missing rebound PR was not refused: %v", err)
					}
					if len(sctx.Agent.(*mockAgent).calls) != 0 {
						t.Fatal("drafted a replacement PR")
					}
				}
				if r.Branch != "refs/heads/feature" || r.PushRef == nil || *r.PushRef != "refs/heads/existing" {
					t.Fatalf("binding/custody = %+v", r)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe rebound push accepted")
				}
				if !strings.Contains(err.Error(), "rebound publication") && !strings.Contains(err.Error(), "refusing") {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if scenario != "missing" && scenario != "deleted-during-push" {
					if got := gitCmd(t, remote, "rev-parse", "refs/heads/existing"); got != before {
						t.Fatal("existing PR history replaced")
					}
				}
				if scenario == "deleted-during-push" {
					if _, err := git.Run(sctx.Ctx, remote, "rev-parse", "--verify", "refs/heads/existing"); err == nil {
						t.Fatal("deleted PR branch was recreated")
					}
				}
			}
			if _, err := git.Run(sctx.Ctx, remote, "rev-parse", "--verify", "refs/heads/feature"); err == nil {
				t.Fatal("publication created a replacement source branch")
			}
		})
	}
}

func TestPublicationHostUsesRegisteredUpstreamWithRepointedOrigin(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", "https://github.com/unrelated/repo")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Env, _ = fakeGH(t, "https://github.com/test/repo/pull/1")
	host, reason := PublicationHost(sctx)
	if host == nil {
		t.Fatal(reason)
	}
	pr, err := host.FindPR(sctx.Ctx, "existing", "")
	if err != nil || pr == nil || pr.URL != "https://github.com/test/repo/pull/1" {
		t.Fatalf("PR proof routed outside registration: %+v %v", pr, err)
	}
	if sctx.Repo.URLsVerified {
		t.Fatal("provider scoping mutated the caller's repository snapshot")
	}
}
