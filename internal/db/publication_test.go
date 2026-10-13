package db

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestPublicationBindingPreservesAuditAndFencesChanges(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/publication", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "source", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding := PublicationBinding{Branch: "destination", PRURL: "https://example.com/pr/1", Target: "digest", SourceRun: run.ID}
	if err := d.UpdateRunPRURL(run.ID, "changed"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishCustody(run, &binding); err == nil {
		t.Fatal("stale PR accepted")
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.FinishCustody(run, &binding); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Publication == nil || got.Publication.Branch != "destination" || *got.PRURL != "changed" || got.LastPushedSHA != nil || got.ReviewApprovedHeadSHA != nil {
		t.Fatalf("binding credited publication/approval or lost audit: %#v", got)
	}
}

func TestPendingRebindDoesNotRetargetHistoricalPushProvenance(t *testing.T) {
	oldRef := "refs/heads/old-publication"
	old := &Run{ID: "old", Branch: "source", PushRef: &oldRef, Publication: &PublicationBinding{SourceRun: "old", Branch: "new-publication"}}
	if old.PublicationBranch() != "old-publication" {
		t.Fatal("pending handoff reinterpreted old publication")
	}
	next := *old
	next.ID = "next"
	next.PushRef = nil
	if next.PublicationBranch() != "new-publication" {
		t.Fatal("successor lost destination")
	}
}
