package daemon

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type custodyTestHost struct {
	scm.Host
	pr     scm.PR
	state  scm.PRState
	onFind func()
}

func (h *custodyTestHost) FindPR(context.Context, string, string) (*scm.PR, error) {
	if h.onFind != nil {
		h.onFind()
	}
	p := h.pr
	return &p, nil
}
func (h *custodyTestHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return h.state, nil
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
	host := &custodyTestHost{pr: scm.PR{URL: prURL, Number: "17", HeadSHA: head, BaseBranch: "main"}, state: scm.PRStateOpen}
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
		if run.ID == f.run.ID || run.ID != validation.RunID || run.HeadSHA != archived || run.Publication == nil || run.Publication.Branch != "destination" || run.ReviewApprovedHeadSHA != nil || run.LastPushedSHA != nil {
			t.Fatalf("successor inherited approval or lost binding: %#v", run)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("successor did not reach fresh review")
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
