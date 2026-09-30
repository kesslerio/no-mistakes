package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublicationBindingFencesAndExcludesAnotherPublisher(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/test", "https://github.com/test/repo", "main")
	run, _ := d.InsertRun(repo.ID, "validation", "head", "base")
	if err := d.RebindPublication(repo, run, "existing", "https://github.com/test/repo/pull/1", "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if err := d.RebindPublication(repo, run, "other", "https://github.com/test/repo/pull/2", "fingerprint"); err == nil {
		t.Fatal("stale binding replaced current destination")
	}
	if _, err := d.InsertRun(repo.ID, "existing", "head", "base"); err == nil {
		t.Fatal("ordinary launch stole rebound publication branch")
	}
	other, err := d.InsertRun(repo.ID, "another-validation", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RebindPublication(repo, other, "existing", "https://github.com/test/repo/pull/1", "fingerprint"); err == nil {
		t.Fatal("second rebound publisher admitted")
	}
	owner, err := d.PublicationOwner(repo.ID, "existing", "")
	if err != nil || owner == nil || owner.ID != run.ID || owner.PublishBranch() != "existing" || owner.Branch != "validation" {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	if owner.LastPushedSHA != nil {
		t.Fatal("destination binding forged successful-push provenance")
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, "existing", "head", "base"); err != nil {
		t.Fatalf("terminal binding retained live lease: %v", err)
	}
}

func TestPublishedCustodyStampFencesGenerationAndAtomicStack(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/test", "https://github.com/test/repo", "main")
	old, _ := d.InsertRun(repo.ID, "feature", "old", "base")
	_ = d.UpdateRunStatus(old.ID, types.RunFailed)
	latest, _ := d.InsertRun(repo.ID, "feature", "new", "base")
	_ = d.UpdateRunStatus(latest.ID, types.RunFailed)
	old, _ = d.GetRun(old.ID)
	latest, _ = d.GetRun(latest.ID)
	_ = d.UpdateRunHeadSHA(old.ID, "changed")
	if err := d.ReleasePublishedCustody(repo, latest, []*Run{latest, old}); err == nil {
		t.Fatal("changed terminal head admitted")
	}
	got, _ := d.GetRun(latest.ID)
	if got.CustodyReturnedAt != nil {
		t.Fatal("partial stack stamped on refusal")
	}
	old, _ = d.GetRun(old.ID)
	if err := d.ReleasePublishedCustody(repo, old, []*Run{old}); err == nil {
		t.Fatal("stale selected generation admitted")
	}
	if err := d.ReleasePublishedCustody(repo, latest, []*Run{latest, old}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Run{latest, old} {
		got, _ := d.GetRun(r.ID)
		if got.CustodyReturnedAt == nil {
			t.Fatal("terminal stack not released")
		}
	}
}

func TestPublicationReservationRejectsReactivationAndOtherBranchOwner(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/test", "https://github.com/test/repo", "main")
	target, _ := d.InsertRun(repo.ID, "existing", "head", "base")
	_ = d.UpdateRunStatus(target.ID, types.RunFailed)
	run, _ := d.InsertRun(repo.ID, "validation", "head", "base")
	if err := d.RebindPublication(repo, run, "existing", "https://github.com/test/repo/pull/1", "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(target.ID, types.RunRunning); err == nil {
		t.Fatal("reactivated source stole publication lease")
	}
	target, _ = d.GetRun(target.ID)
	if err := d.ReleasePublishedCustody(repo, target, nil); err == nil {
		t.Fatal("custody released against another live publisher")
	}
}
