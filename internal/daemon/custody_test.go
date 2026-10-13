package daemon

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type custodyTestHost struct {
	scm.Host
	pr       scm.PR
	state    scm.PRState
	onFind   func()
	repoURL  string
	branch   string
	identity *scm.PRHeadIdentity
}

func (h *custodyTestHost) FindPR(_ context.Context, branch, _ string) (*scm.PR, error) {
	h.branch = branch
	if h.onFind != nil {
		h.onFind()
	}
	p := h.pr
	return &p, nil
}
func (h *custodyTestHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return h.state, nil
}

func (h *custodyTestHost) ReadPRHeadIdentity(context.Context, *scm.PR) (scm.PRHeadIdentity, error) {
	if h.identity != nil {
		return *h.identity, nil
	}
	return scm.PRHeadIdentity{RepositoryURL: h.repoURL, Branch: h.branch, SHA: h.pr.HeadSHA}, nil
}

type custodyFixture struct {
	m                      *RunManager
	p                      *paths.Paths
	d                      *db.DB
	repo                   *db.Repo
	run                    *db.Run
	head, gate, remote, wt string
	host                   *custodyTestHost
	params                 ipc.CustodyParams
}

func newCustodyFixture(t *testing.T) *custodyFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, head := setupTestGitRepo(t, p, d, "custody")
	gate := p.RepoDir(repo.ID)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCmd(t, "", "clone", "--bare", gate, remote)
	gitCmd(t, gate, "update-ref", "refs/heads/source", head)
	gitCmd(t, remote, "update-ref", "refs/heads/source", head)
	gitCmd(t, remote, "update-ref", "refs/heads/destination", head)
	repo, err = d.UpdateRepoMetadata(repo.ID, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "source", head, head)
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/test/repo/pull/17"
	if err := d.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatusWithVerifiedHead(run.ID, "daemon shutting down", types.RunFailed, head); err != nil {
		t.Fatal(err)
	}
	wt := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, gate, "worktree", "add", "--detach", wt, head)
	m := NewRunManager(d, p, nil)
	host := &custodyTestHost{pr: scm.PR{URL: prURL, Number: "17", HeadSHA: head, BaseBranch: "main"}, state: scm.PRStateOpen, repoURL: remote}
	m.custodyHost = host
	return &custodyFixture{m: m, p: p, d: d, repo: repo, run: run, head: head, gate: gate, remote: remote, wt: wt, host: host, params: ipc.CustodyParams{RepoID: repo.ID, Branch: "source", ExpectedRun: run.ID, Action: "release"}}
}
func TestCustodyReleaseAndRestartReconcile(t *testing.T) {
	for _, action := range []string{"release", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			f := newCustodyFixture(t)
			f.params.Action = action
			got, err := f.m.HandleCustody(context.Background(), f.params)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != "custody-returned" {
				t.Fatal(got)
			}
			r, _ := f.d.GetRun(f.run.ID)
			if r.CustodyReturnedAt == nil || r.PRURL == nil || *r.PRURL != f.host.pr.URL || r.LastPushedSHA != nil {
				t.Fatalf("lost audit or invented push: %#v", r)
			}
			archive := gitOutput(t, f.gate, "rev-parse", "refs/no-mistakes/custody/"+r.ID+"/"+f.head)
			if archive != f.head {
				t.Fatal(archive)
			}
		})
	}
}
func TestCustodyRefusalsLeaveRunAndGateIntact(t *testing.T) {
	cases := map[string]func(*testing.T, *custodyFixture){
		"default source": func(t *testing.T, f *custodyFixture) { f.params.Branch = "main" },
		"replacement PR": func(t *testing.T, f *custodyFixture) { f.host.pr.URL = "https://github.com/test/repo/pull/18" },
		"closed PR":      func(t *testing.T, f *custodyFixture) { f.host.state = scm.PRStateClosed },
		"missing branch": func(t *testing.T, f *custodyFixture) { gitCmd(t, f.remote, "update-ref", "-d", "refs/heads/source") },
		"dirty worktree": func(t *testing.T, f *custodyFixture) {
			if err := os.WriteFile(filepath.Join(f.wt, "untracked"), []byte("precious"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"stale run": func(t *testing.T, f *custodyFixture) { f.params.ExpectedRun = "stale" },
		"wrong historical error": func(t *testing.T, f *custodyFixture) {
			f.params.Action = "reconcile"
			if err := f.d.UpdateRunErrorStatus(f.run.ID, "test failed", types.RunFailed); err != nil {
				t.Fatal(err)
			}
		},
		"unpublished head": func(t *testing.T, f *custodyFixture) {
			gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "unpublished")
		},
		"archive refusal": func(t *testing.T, f *custodyFixture) {
			ref := "refs/no-mistakes/custody/" + f.run.ID + "/" + f.head
			gitCmd(t, f.gate, "symbolic-ref", ref, "refs/heads/main")
		},
		"destination owned": func(t *testing.T, f *custodyFixture) {
			f.params.Action = "rebind"
			f.params.Destination = "destination"
			f.params.PRURL = f.host.pr.URL
			r, e := f.d.InsertRun(f.repo.ID, "destination", f.head, f.head)
			if e != nil {
				t.Fatal(e)
			}
			if e = f.d.UpdateRunStatus(r.ID, types.RunRunning); e != nil {
				t.Fatal(e)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCustodyFixture(t)
			setup(t, f)
			before, _ := f.d.GetRun(f.run.ID)
			if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
				t.Fatal("unexpected success")
			}
			after, _ := f.d.GetRun(f.run.ID)
			if after.CustodyReturnedAt != nil || after.Publication != nil || after.Status != before.Status {
				t.Fatalf("refusal changed ownership: %#v", after)
			}
			if got := gitOutput(t, f.gate, "rev-parse", "refs/heads/source"); got != f.head {
				t.Fatal("refusal changed gate")
			}
		})
	}
}
func TestCustodyRebindQuiescesWithoutApprovalOrPublicationCredit(t *testing.T) {
	f := newCustodyFixture(t)
	f.params.Action = "rebind"
	f.params.Destination = "destination"
	f.params.PRURL = f.host.pr.URL
	if err := f.d.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	f.m.cancels[f.run.ID] = cancel
	f.m.dones[f.run.ID] = done
	go func() {
		<-ctx.Done()
		_ = f.d.UpdateRunErrorStatus(f.run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled)
		close(done)
	}()
	got, err := f.m.HandleCustody(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "rerun-required" {
		t.Fatal(got)
	}
	r, _ := f.d.GetRun(f.run.ID)
	if r.Publication == nil || r.Publication.Branch != "destination" || r.LastPushedSHA != nil || r.ReviewApprovedHeadSHA != nil {
		t.Fatalf("bad binding %#v", r)
	}
	if !strings.Contains(protectedPathCleanupReason(f.d, r), "handoff") {
		t.Fatal("worktree not protected")
	}
	if gitOutput(t, f.remote, "rev-parse", "refs/heads/destination") != f.head {
		t.Fatal("rebind wrote remote")
	}
}

func TestCustodyQuiescenceRacesFailClosed(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(map[bool]string{false: "head changes", true: "dirty during stop"}[dirty], func(t *testing.T) {
			f := newCustodyFixture(t)
			if err := f.d.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			done := make(chan struct{})
			f.m.cancels[f.run.ID] = cancel
			f.m.dones[f.run.ID] = done
			go func() {
				<-ctx.Done()
				if dirty {
					_ = os.WriteFile(filepath.Join(f.wt, "raced"), []byte("preserve me"), 0o644)
				} else {
					gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "raced")
				}
				_ = f.d.UpdateRunErrorStatus(f.run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled)
				close(done)
			}()
			if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
				t.Fatal("raced release succeeded")
			}
			r, _ := f.d.GetRun(f.run.ID)
			if r.CustodyReturnedAt != nil {
				t.Fatal("raced custody stamp")
			}
			if _, err := os.Stat(f.wt); err != nil {
				t.Fatal("worktree was lost", err)
			}
			if !dirty {
				head := gitOutput(t, f.wt, "rev-parse", "HEAD")
				if gitOutput(t, f.gate, "rev-parse", "refs/no-mistakes/custody/"+r.ID+"/"+head) != head {
					t.Fatal("raced head not archived")
				}
			}
		})
	}
}

func TestCustodyDestinationChangesDuringQuiescence(t *testing.T) {
	f := newCustodyFixture(t)
	calls := 0
	f.host.onFind = func() {
		calls++
		if calls == 2 {
			f.host.pr.URL = "https://github.com/test/repo/pull/18"
		}
	}
	if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
		t.Fatal("replacement raced release")
	}
	r, _ := f.d.GetRun(f.run.ID)
	if r.CustodyReturnedAt != nil {
		t.Fatal("stamp despite changed PR")
	}
}

type custodySuccessorStep struct{ observed chan *db.Run }

func (s *custodySuccessorStep) Name() types.StepName { return types.StepReview }
func (s *custodySuccessorStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	copy := *ctx.Run
	s.observed <- &copy
	return &pipeline.StepOutcome{}, nil
}
func TestCustodyReboundSuccessorStartsWithNoApproval(t *testing.T) {
	f := newCustodyFixture(t)
	f.params.Action = "rebind"
	f.params.Destination = "destination"
	f.params.PRURL = f.host.pr.URL
	gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "preserved unpublished successor work")
	archived := gitOutput(t, f.wt, "rev-parse", "HEAD")
	if _, err := f.m.HandleCustody(context.Background(), f.params); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.HandleRerun(context.Background(), f.repo.ID, "source", "", []types.StepName{types.StepReview}, "", "", false, archived, ""); err == nil {
		t.Fatal("rebound successor skipped validation")
	}
	observed := make(chan *db.Run, 1)
	f.m.steps = func() []pipeline.Step { return []pipeline.Step{&custodySuccessorStep{observed: observed}} }
	bin := t.TempDir()
	writeMockClaude(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.params.Action = "validate"
	f.params.Head = archived
	validation, err := f.m.HandleCustody(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	defer f.m.Shutdown()
	select {
	case run := <-observed:
		if run.ID == f.run.ID || run.ID != validation.RunID || run.HeadSHA != archived || run.Publication == nil || run.Publication.Branch != "destination" || run.ReviewApprovedHeadSHA != nil || run.LastPushedSHA != nil || run.PRBaseBranch != nil {
			t.Fatalf("successor inherited approval or lost binding: %#v", run)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("successor did not reach fresh review")
	}
	f.m.wg.Wait()
	gitCmd(t, f.gate, "config", "remote.origin.url", f.remote)
	gitCmd(t, f.remote, "update-ref", "refs/heads/release", f.head)
	if _, err := f.m.HandleRerun(context.Background(), f.repo.ID, "source", "", nil, "", "release", false, archived, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case run := <-observed:
		if run.PRBaseBranch == nil || *run.PRBaseBranch != "release" {
			t.Fatalf("explicit rerun base lost: %#v", run.PRBaseBranch)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("explicit-base rerun did not reach review")
	}
	f.m.wg.Wait()
}

func TestCustodyRefusesGateMoveWhileStopping(t *testing.T) {
	f := newCustodyFixture(t)
	gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "other head")
	other := gitOutput(t, f.wt, "rev-parse", "HEAD")
	gitCmd(t, f.wt, "checkout", "--detach", f.head)
	if err := f.d.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	f.m.cancels[f.run.ID] = cancel
	f.m.dones[f.run.ID] = done
	go func() {
		<-ctx.Done()
		gitCmd(t, f.gate, "update-ref", "refs/heads/source", other)
		_ = f.d.UpdateRunErrorStatus(f.run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled)
		close(done)
	}()
	if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
		t.Fatal("concurrent gate move accepted")
	}
	if gitOutput(t, f.gate, "rev-parse", "refs/heads/source") != other {
		t.Fatal("concurrent gate was overwritten")
	}
	r, _ := f.d.GetRun(f.run.ID)
	if r.CustodyReturnedAt != nil {
		t.Fatal("custody returned despite concurrent gate change")
	}
}

func TestCustodyHoldPreventsCrashResume(t *testing.T) {
	f := newCustodyFixture(t)
	if err := f.d.HoldCustodyWorktree(f.run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.prepareRecoveredRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "custody handoff") {
		t.Fatalf("held handoff resumed: %v", err)
	}
}

func TestCustodyRebindPreservesUnpublishedDescendant(t *testing.T) {
	f := newCustodyFixture(t)
	f.params.Action = "rebind"
	f.params.Destination = "destination"
	f.params.PRURL = f.host.pr.URL
	gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "unpublished work")
	head := gitOutput(t, f.wt, "rev-parse", "HEAD")
	result, err := f.m.HandleCustody(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	if result.Head != head || gitOutput(t, f.gate, "rev-parse", "refs/heads/source") != head {
		t.Fatal("lost unpublished descendant")
	}
	if gitOutput(t, f.gate, "rev-parse", "refs/no-mistakes/custody/"+f.run.ID+"/"+head) != head {
		t.Fatal("unpublished descendant lacks archive")
	}
	if gitOutput(t, f.remote, "rev-parse", "refs/heads/destination") != f.head {
		t.Fatal("rebind published without validation")
	}
}

func TestCustodyValidateRefusesUnboundOrMovedHead(t *testing.T) {
	for _, scenario := range []string{"unbound", "wrong-head", "moved-gate", "newer-run"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCustodyFixture(t)
			if scenario != "unbound" {
				f.params.Action = "rebind"
				f.params.Destination = "destination"
				f.params.PRURL = f.host.pr.URL
				if _, err := f.m.HandleCustody(context.Background(), f.params); err != nil {
					t.Fatal(err)
				}
			}
			f.params.Action = "validate"
			f.params.Head = f.head
			switch scenario {
			case "wrong-head":
				f.params.Head = strings.Repeat("a", 40)
			case "moved-gate":
				gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "other")
				gitCmd(t, f.gate, "update-ref", "refs/heads/source", gitOutput(t, f.wt, "rev-parse", "HEAD"))
			case "newer-run":
				if _, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
				t.Fatal("unsafe validation selection accepted")
			}
		})
	}
}

func TestCustodySettlesTerminalStack(t *testing.T) {
	for _, action := range []string{"release", "reconcile", "rebind"} {
		t.Run(action, func(t *testing.T) {
			f := newCustodyFixture(t)
			gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "unpublished custody work")
			head := gitOutput(t, f.wt, "rev-parse", "HEAD")
			if err := f.d.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "failure", types.RunFailed, head); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.gate, "update-ref", "refs/heads/source", head)
			gitCmd(t, f.remote, "fetch", f.gate, head)
			gitCmd(t, f.remote, "update-ref", "refs/heads/source", head)
			gitCmd(t, f.remote, "update-ref", "refs/heads/destination", head)
			f.host.pr.HeadSHA = head
			f.head = head
			prior, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunErrorStatusWithVerifiedHead(prior.ID, "daemon shutting down", types.RunFailed, f.head); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunPRURL(prior.ID, f.host.pr.URL); err != nil {
				t.Fatal(err)
			}
			f.params.ExpectedRun = prior.ID
			f.params.Action = action
			f.params.Destination = "destination"
			f.params.PRURL = f.host.pr.URL
			if _, err := f.m.HandleCustody(context.Background(), f.params); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{prior.ID, f.run.ID} {
				run, err := f.d.GetRun(id)
				if err != nil || run.CustodyReturnedAt == nil {
					t.Fatalf("custody remains: %s %v", id, err)
				}
			}
		})
	}
}

func TestCustodyFreshLaunchDoesNotInheritDestination(t *testing.T) {
	f := newCustodyFixture(t)
	f.params.Action = "rebind"
	f.params.Destination = "destination"
	f.params.PRURL = f.host.pr.URL
	if _, err := f.m.HandleCustody(context.Background(), f.params); err != nil {
		t.Fatal(err)
	}
	successor, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	binding := db.PublicationBinding{Branch: "destination", PRURL: f.host.pr.URL, Target: "target", SourceRun: f.run.ID}
	if err := f.d.SetRunPublicationDestination(successor.ID, binding); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunPRURL(successor.ID, f.host.pr.URL); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(successor.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	observed := make(chan *db.Run, 1)
	f.m.steps = func() []pipeline.Step { return []pipeline.Step{&custodySuccessorStep{observed: observed}} }
	bin := t.TempDir()
	writeMockClaude(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.m.startFreshLaunch(context.Background(), f.repo, "source", f.head, "", f.gate, nil, "fresh source validation", "fresh-custody-launch", "fresh-custody-generation", "", false, "push", "")
	if err != nil {
		t.Fatal(err)
	}
	defer f.m.Shutdown()
	select {
	case run := <-observed:
		if run.Publication != nil || run.PRURL != nil {
			t.Fatalf("fresh launch inherited destination: %#v", run)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fresh launch did not reach review")
	}
	f.m.wg.Wait()
}

func TestCustodyRefusesUncontainedTerminalStack(t *testing.T) {
	f := newCustodyFixture(t)
	gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "orphaned older work")
	unpublished := gitOutput(t, f.wt, "rev-parse", "HEAD")
	gitCmd(t, f.wt, "checkout", "--detach", f.head)
	if err := f.d.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "failure", types.RunFailed, unpublished); err != nil {
		t.Fatal(err)
	}
	latest, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunErrorStatusWithVerifiedHead(latest.ID, "daemon shutting down", types.RunFailed, f.head); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunPRURL(latest.ID, f.host.pr.URL); err != nil {
		t.Fatal(err)
	}
	f.params.ExpectedRun = latest.ID
	if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
		t.Fatal("orphaned older work accepted")
	}
	for _, id := range []string{latest.ID, f.run.ID} {
		run, err := f.d.GetRun(id)
		if err != nil || run.CustodyReturnedAt != nil {
			t.Fatalf("custody incorrectly returned: %s %v", id, err)
		}
	}
	if gitOutput(t, f.gate, "rev-parse", "refs/heads/source") != f.head {
		t.Fatal("gate moved on refusal")
	}
}

func TestCustodyPreviouslyPublishedHistoryDoesNotBlockHandoff(t *testing.T) {
	for _, action := range []string{"release", "reconcile", "rebind"} {
		t.Run(action, func(t *testing.T) {
			f := newCustodyFixture(t)
			gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "published H1")
			h1 := gitOutput(t, f.wt, "rev-parse", "HEAD")
			if err := f.d.UpdateRunPublication(f.run.ID, db.PushBinding{HeadSHA: h1, TargetKind: "upstream", Ref: "refs/heads/source"}); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.remote, "fetch", f.gate, h1)
			gitCmd(t, f.remote, "update-ref", "refs/heads/source", h1)
			gitCmd(t, f.wt, "checkout", "--detach", f.head)
			gitCmd(t, f.wt, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "author rewritten H2")
			h2 := gitOutput(t, f.wt, "rev-parse", "HEAD")
			gitCmd(t, f.wt, "checkout", "--detach", h1)
			gitCmd(t, f.gate, "update-ref", "refs/heads/source", h2)
			gitCmd(t, f.remote, "fetch", f.gate, h2)
			gitCmd(t, f.remote, "update-ref", "refs/heads/source", h2)
			gitCmd(t, f.remote, "update-ref", "refs/heads/destination", h2)
			f.host.pr.HeadSHA = h2
			latest, err := f.d.InsertRun(f.repo.ID, "source", h2, f.head)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunPublication(latest.ID, db.PushBinding{HeadSHA: h2, TargetKind: "upstream", Ref: "refs/heads/source"}); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunErrorStatusWithVerifiedHead(latest.ID, "daemon shutting down", types.RunFailed, h2); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunPRURL(latest.ID, f.host.pr.URL); err != nil {
				t.Fatal(err)
			}
			f.params.ExpectedRun = latest.ID
			f.params.Action = action
			f.params.Destination = "destination"
			f.params.PRURL = f.host.pr.URL
			if _, err := f.m.HandleCustody(context.Background(), f.params); err != nil {
				t.Fatal(err)
			}
			old, err := f.d.GetRun(f.run.ID)
			if err != nil || old.CustodyReturnedAt != nil {
				t.Fatal("published history was included in outstanding custody")
			}
			current, err := f.d.GetRun(latest.ID)
			if err != nil || current.CustodyReturnedAt == nil {
				t.Fatal("latest custody not returned")
			}
		})
	}
}

func TestCustodyUsesOnlyMatchingOriginCredentials(t *testing.T) {
	for _, action := range []string{"release", "reconcile", "rebind"} {
		for _, matching := range []bool{true, false} {
			t.Run(action+fmt.Sprint(matching), func(t *testing.T) {
				f := newCustodyFixture(t)
				credentialURL := "https://test-user:test-password@example.com/team/repo.git"
				registered := safeurl.Redact(credentialURL)
				origin := credentialURL
				if !matching {
					origin = "https://test-user:test-password@example.com/other/repo.git"
				}
				gitCmd(t, f.gate, "config", "remote.origin.url", origin)
				gitCmd(t, f.gate, "config", "url."+f.remote+".insteadOf", credentialURL)
				missing := filepath.Join(t.TempDir(), "missing.git")
				gitCmd(t, f.gate, "config", "url."+missing+".insteadOf", registered)
				var err error
				f.repo, err = f.d.UpdateRepoMetadata(f.repo.ID, registered, "main")
				f.host.repoURL = registered
				if err != nil {
					t.Fatal(err)
				}
				f.params.Action = action
				f.params.Destination = "destination"
				f.params.PRURL = f.host.pr.URL
				_, err = f.m.HandleCustody(context.Background(), f.params)
				if matching && err != nil {
					t.Fatal(err)
				}
				if !matching && err == nil {
					t.Fatal("unmatched origin credentials used")
				}
			})
		}
	}
}

func TestCustodyRefusesWrongPRHeadIdentity(t *testing.T) {
	for _, action := range []string{"release", "reconcile", "rebind"} {
		for _, mismatch := range []string{"repository", "branch", "commit"} {
			t.Run(action+"/"+mismatch, func(t *testing.T) {
				f := newCustodyFixture(t)
				branch := "source"
				if action == "rebind" {
					branch = "destination"
				}
				identity := scm.PRHeadIdentity{RepositoryURL: f.remote, Branch: branch, SHA: f.head}
				switch mismatch {
				case "repository":
					identity.RepositoryURL = f.remote + "-fork"
				case "branch":
					identity.Branch = "other"
				case "commit":
					identity.SHA = "other"
				}
				f.host.identity = &identity
				f.params.Action = action
				f.params.Destination = "destination"
				f.params.PRURL = f.host.pr.URL
				if _, err := f.m.HandleCustody(context.Background(), f.params); err == nil {
					t.Fatal("wrong PR identity accepted")
				}
				run, err := f.d.GetRun(f.run.ID)
				if err != nil || run.CustodyReturnedAt != nil {
					t.Fatal("identity refusal returned custody")
				}
				if gitOutput(t, f.gate, "rev-parse", "refs/heads/source") != f.head {
					t.Fatal("identity refusal moved gate")
				}
			})
		}
	}
}

func TestCustodySelectedRerunKeepsPublicationAndPRTogether(t *testing.T) {
	for _, selectedBound := range []bool{true, false} {
		t.Run(fmt.Sprint(selectedBound), func(t *testing.T) {
			f := newCustodyFixture(t)
			binding := db.PublicationBinding{Branch: "destination", PRURL: f.host.pr.URL, Target: "target", SourceRun: f.run.ID}
			selected, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head)
			if err != nil {
				t.Fatal(err)
			}
			latest, err := f.d.InsertRun(f.repo.ID, "source", f.head, f.head)
			if err != nil {
				t.Fatal(err)
			}
			bound := latest
			if selectedBound {
				bound = selected
			}
			if err := f.d.SetRunPublicationDestination(bound.ID, binding); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunPRURL(bound.ID, binding.PRURL); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunPublication(bound.ID, db.PushBinding{HeadSHA: f.head, Ref: "refs/heads/destination", TargetKind: "upstream"}); err != nil {
				t.Fatal(err)
			}
			for _, prior := range []*db.Run{selected, latest} {
				if err := f.d.UpdateRunStatus(prior.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			}
			observed := make(chan *db.Run, 1)
			f.m.steps = func() []pipeline.Step { return []pipeline.Step{&custodySuccessorStep{observed: observed}} }
			bin := t.TempDir()
			writeMockClaude(t, bin)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: claude\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			id, err := f.m.HandleRerun(context.Background(), f.repo.ID, "source", selected.ID, nil, "", "", false, f.head, "")
			if err != nil {
				t.Fatal(err)
			}
			defer f.m.Shutdown()
			select {
			case run := <-observed:
				persisted, err := f.d.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				for _, actual := range []*db.Run{run, persisted} {
					if selectedBound {
						if actual.Publication == nil || *actual.Publication != binding || actual.PRURL == nil || *actual.PRURL != binding.PRURL || actual.PublicationBranch() != "destination" {
							t.Fatalf("selected binding lost: %#v", actual)
						}
					} else if actual.Publication != nil || actual.PRURL != nil || actual.PublicationBranch() != "source" {
						t.Fatalf("latest binding leaked: %#v", actual)
					}
				}
			case <-time.After(10 * time.Second):
				t.Fatal("selected rerun did not reach review")
			}
			f.m.wg.Wait()
		})
	}
}
