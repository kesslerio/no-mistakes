package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newPublishedReleaseFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	f.service.GateDir = filepath.Join(filepath.Dir(f.local), "gate.git")
	mustRun(t, filepath.Dir(f.local), "clone", "--bare", f.remote, f.service.GateDir)
	mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
	mustRun(t, f.local, "merge", "--ff-only", "FETCH_HEAD")
	// The gate contains a stranded unpublished descendant while the operator
	// and open PR retain the last published head.
	mustRun(t, f.local, "commit", "--allow-empty", "-m", "unpublished pipeline work")
	stranded := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.service.GateDir, "fetch", f.local, "HEAD")
	mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", stranded)
	mustRun(t, f.local, "reset", "--hard", f.pushed)
	if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "daemon shutting down", types.RunFailed, stranded); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	return f
}

func TestReleasePublishedArchivesAndRestoresMissingOrStaleGate(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale", true: "missing"}[missing], func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			if missing {
				mustRun(t, f.service.GateDir, "update-ref", "-d", "refs/heads/feature/sync")
			}
			before := f.run.HeadSHA
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, true, releasePRProof)
			if !state.Recovered || state.State != StateCustodyReturned {
				t.Fatalf("release: %+v", state)
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, before)); got != before {
				t.Fatalf("archive = %s", got)
			}
			if f.run.SubmittedHeadSHA != nil {
				if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, *f.run.SubmittedHeadSHA)); got != *f.run.SubmittedHeadSHA {
					t.Fatal("submitted head was not preserved")
				}
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("gate = %s", got)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
				t.Fatalf("caller changed: %s", got)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("remote changed: %s", got)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt == nil || run.HeadSHA != before || ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) {
				t.Fatalf("lost historical provenance: %+v", run)
			}
			again := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, true, releasePRProof)
			if !again.Recovered || again.Changed {
				t.Fatalf("retry not idempotent: %+v", again)
			}
		})
	}
}

func TestReleasePublishedAdoptsDivergedPRHeadWithoutChangingPushHistory(t *testing.T) {
	t.Parallel()
	f := newPublishedReleaseFixture(t)
	// A separately published replacement can differ from both the old push
	// binding and the stranded pipeline head. Adoption preserves all three.
	mustRun(t, f.local, "reset", "--hard", f.base)
	mustRun(t, f.local, "commit", "--allow-empty", "-m", "separately published replacement")
	published := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.local, "push", f.remote, published+":refs/heads/feature/sync", "--force-with-lease=refs/heads/feature/sync:"+f.pushed)
	state := f.service.ReleasePublished(f.ctx, f.run.ID, published, true, releasePRProof)
	if !state.Recovered || state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("diverged publication was not adopted: %+v", state)
	}
	for _, head := range []string{published, f.run.HeadSHA, f.pushed} {
		if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, head)); got != head {
			t.Fatalf("lost preserved head %s: %s", head, got)
		}
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != published {
		t.Fatalf("fresh-run gate head = %s, want %s", got, published)
	}
	run, _ := f.db.GetRun(f.run.ID)
	if run.CustodyReturnedAt == nil || run.HeadSHA != f.run.HeadSHA || ptr(run.LastPushedSHA) != f.pushed {
		t.Fatalf("lost historical provenance: %+v", run)
	}
	if inspected := f.service.InspectCached(f.ctx); inspected.State == StatePipelineOwned || inspected.State == StatePushInProgress {
		t.Fatalf("old run still blocks fresh adoption: %+v", inspected)
	}
}

func TestReleasePublishedRefusesUnsafePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*testing.T, *syncFixture)
		reason string
	}{
		{"unpublished-local", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.local, "commit", "--allow-empty", "-m", "unpublished local")
		}, "exactly match"},
		{"missing-publication", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.remote, "update-ref", "-d", "refs/heads/feature/sync")
		}, "exactly match"},
		{"dirty-caller", func(t *testing.T, f *syncFixture) { mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted") }, "clean"},
		{"active-run", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		}, "live run"},
		{"missing-unpublished-head", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunHeadSHA(f.run.ID, strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
		}, "unavailable"},
		{"conflicting-archive", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "update-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), f.pushed)
		}, "archive conflicts"},
		{"symbolic-archive", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "symbolic-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), "refs/heads/feature/sync")
		}, "archive conflicts"},
		{"symbolic-gate", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "update-ref", "refs/heads/alias", f.run.HeadSHA)
			mustRun(t, f.service.GateDir, "symbolic-ref", "refs/heads/feature/sync", "refs/heads/alias")
		}, "symbolic"},
		{"nonrestart-failure", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "lint failed", types.RunFailed, f.run.HeadSHA); err != nil {
				t.Fatal(err)
			}
		}, "historical daemon"},
		{"newer-generation", func(t *testing.T, f *syncFixture) {
			r, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
		}, "current branch generation"},
		{"dirty-managed", func(t *testing.T, f *syncFixture) {
			if err := f.db.SetRunWorktreeDir(f.run.ID, f.local); err != nil {
				t.Fatal(err)
			}
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted")
				return git.LsRemote(ctx, dir, remote, ref)
			}
		}, "uncommitted work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			tc.change(t, f)
			gateBefore, _, _ := git.ExactRefTarget(f.ctx, f.service.GateDir, "refs/heads/feature/sync")
			head := mustRun(t, f.local, "rev-parse", "HEAD")
			state := f.service.ReleasePublished(f.ctx, f.run.ID, head, true, releasePRProof)
			if state.Recovered || !strings.Contains(state.Error, tc.reason) {
				t.Fatalf("refusal: %+v; want %q", state, tc.reason)
			}
			gateAfter, _, _ := git.ExactRefTarget(f.ctx, f.service.GateDir, "refs/heads/feature/sync")
			if gateBefore != gateAfter {
				t.Fatal("refused operation replaced gate ref")
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt != nil {
				t.Fatal("refused operation released custody")
			}
		})
	}
}

func TestReleasePublishedDoesNotOverwriteGateRace(t *testing.T) {
	t.Parallel()
	f := newPublishedReleaseFixture(t)
	n := 0
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		n++
		if n == 2 {
			mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", f.base)
		}
		return git.LsRemote(ctx, dir, remote, ref)
	}
	state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof)
	if state.Recovered || !strings.Contains(state.Error, "gate lane changed") {
		t.Fatalf("race: %+v", state)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.base {
		t.Fatalf("other owner clobbered: %s", got)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, f.run.HeadSHA)); got != f.run.HeadSHA {
		t.Fatalf("old head not archived: %s", got)
	}
}

// Keep the recovery namespace independent: release never replaces a prior
// recovery anchor just to make a diverged published branch usable.
func TestReleasePublishedPreservesPriorRecoveryEvidence(t *testing.T) {
	t.Parallel()
	f := newPublishedReleaseFixture(t)
	mustRun(t, f.service.GateDir, "update-ref", custody.RecoveryRef(f.run.ID), f.base)
	if state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof); !state.Recovered {
		t.Fatalf("%+v", state)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", custody.RecoveryRef(f.run.ID)); got != f.base {
		t.Fatal("recovery evidence overwritten")
	}
}

func TestReleasePublishedRechecksArchivesAndManagedWork(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"deleted-archive", "symbolic-archive", "changed-managed", "changed-generation", "changed-error", "changed-default", "changed-registration"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			if scenario == "changed-managed" {
				if err := f.db.SetRunWorktreeDir(f.run.ID, f.local); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				n++
				if n == 2 {
					switch scenario {
					case "symbolic-archive":
						mustRun(t, f.service.GateDir, "symbolic-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), "refs/heads/feature/sync")
					case "deleted-archive":
						mustRun(t, f.service.GateDir, "update-ref", "-d", releaseArchiveRef(f.run.ID, f.run.HeadSHA))
					case "changed-managed":
						mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted work")
					case "changed-default":
						if _, err := f.db.UpdateRepoMetadata(f.repo.ID, f.repo.UpstreamURL, f.run.Branch); err != nil {
							t.Fatal(err)
						}
					case "changed-registration":
						if _, err := f.db.UpdateRepoWorkingPath(f.repo.ID, filepath.Join(f.local, "different")); err != nil {
							t.Fatal(err)
						}
					case "changed-error":
						if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "lint failed", types.RunFailed, f.run.HeadSHA); err != nil {
							t.Fatal(err)
						}
					case "changed-generation":
						if err := f.db.UpdateRunHeadSHA(f.run.ID, f.base); err != nil {
							t.Fatal(err)
						}
					}
				}
				return git.LsRemote(ctx, dir, remote, ref)
			}
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof)
			if state.Recovered {
				t.Fatalf("unsafe recovery: %+v", state)
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.run.HeadSHA {
				t.Fatal("unarchived or stale generation gate head dropped")
			}
			r, _ := f.db.GetRun(f.run.ID)
			if r.CustodyReturnedAt != nil {
				t.Fatal("unsafe generation stamped")
			}
		})
	}
}

func releasePRProof(context.Context) error { return nil }
