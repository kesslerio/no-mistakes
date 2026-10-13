package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// HandleCustody is an explicit operator handoff, not an approval. The source
// executor must be gone before its destination can change. Holds survive a
// daemon crash and keep cancellation cleanup from deleting raced work.
func (m *RunManager) HandleCustody(ctx context.Context, p ipc.CustodyParams) (*ipc.CustodyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if p.Action == "validate" {
		return m.validateCustodySuccessor(ctx, p)
	}
	var result *ipc.CustodyResult
	_, err := m.withBranchLock(p.RepoID, p.Branch, func() (string, error) {
		lock := m.publicationLock(p.RepoID)
		lock.Lock()
		defer lock.Unlock()
		var err error
		result, err = m.custodyLocked(ctx, p)
		return "", err
	})
	return result, err
}

func (m *RunManager) custodyLocked(ctx context.Context, p ipc.CustodyParams) (*ipc.CustodyResult, error) {
	if p.Action != "release" && p.Action != "rebind" && p.Action != "reconcile" {
		return nil, fmt.Errorf("unknown custody action")
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown repository")
	}
	if p.Branch == repo.DefaultBranch {
		return nil, fmt.Errorf("custody handoffs refuse the default branch")
	}
	runs, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, err
	}
	var run *db.Run
	for _, r := range runs {
		if r.Branch == p.Branch {
			run = r
			break
		}
	}
	if run == nil || run.ID != p.ExpectedRun {
		return nil, fmt.Errorf("source run changed; inspect status and retry")
	}
	if p.Action == "reconcile" && (run.Status != types.RunFailed || run.Error == nil || (*run.Error != "daemon shutting down" && *run.Error != "daemon crashed during execution")) {
		return nil, fmt.Errorf("reconcile requires an exact historical daemon-shutdown failure")
	}
	destination := run.PublicationBranch()
	if p.Action == "rebind" {
		destination = p.Destination
	}
	if destination == "" || destination == repo.DefaultBranch {
		return nil, fmt.Errorf("destination must be an existing non-default branch")
	}
	gate := m.paths.RepoDir(repo.ID)
	if _, err := git.Run(ctx, gate, "check-ref-format", "refs/heads/"+destination); err != nil {
		return nil, fmt.Errorf("invalid destination branch")
	}
	for _, r := range runs {
		if r.ID != run.ID && (r.Status == types.RunRunning || r.Status == types.RunPending) && (r.Branch == p.Branch || r.Branch == destination || r.PublicationBranch() == destination) {
			return nil, fmt.Errorf("destination is owned by run %s", r.ID)
		}
	}
	cfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, err
	}
	sctx := &pipeline.StepContext{Ctx: ctx, DB: m.db, Run: run, Repo: repo, Config: config.Merge(cfg, &config.RepoConfig{}), WorkDir: gate, ForgeContext: forge, Log: func(string) {}}
	host := m.custodyHost
	if host == nil {
		host, err = steps.PublicationHost(sctx)
	}
	if err != nil {
		return nil, err
	}
	pushURL := repo.PushURL()
	if origin, err := git.GetConfiguredRemoteURL(ctx, gate, "origin"); err == nil && branchsync.TargetFingerprint(origin) == branchsync.TargetFingerprint(pushURL) {
		pushURL = origin
	}
	expectedPR := p.PRURL
	if p.Action != "rebind" {
		if run.PRURL == nil {
			return nil, fmt.Errorf("release requires the recorded PR identity")
		}
		expectedPR = *run.PRURL
	}
	if expectedPR == "" {
		return nil, fmt.Errorf("an exact destination PR URL is required")
	}
	verify := func() (string, string, error) {
		pr, e := host.FindPR(ctx, destination, "")
		if e != nil {
			return "", "", e
		}
		if pr == nil || pr.URL != expectedPR {
			return "", "", fmt.Errorf("existing destination PR is absent or replaced")
		}
		state, e := host.GetPRState(ctx, pr)
		if e != nil {
			return "", "", e
		}
		if state != "OPEN" {
			return "", "", fmt.Errorf("destination PR is not open")
		}
		remoteCtx, cancel := context.WithTimeout(ctx, config.DefaultBranchSyncRemoteTimeout)
		defer cancel()
		head, e := git.LsRemote(remoteCtx, gate, pushURL, "refs/heads/"+destination)
		if e != nil {
			return "", "", e
		}
		if head == "" {
			return "", "", fmt.Errorf("destination branch does not exist")
		}
		if e := scm.VerifyPRHeadIdentity(ctx, host, pr, repo.PushURL(), destination, head); e != nil {
			return "", "", e
		}
		return head, pr.BaseBranch, nil
	}
	live, base, err := verify()
	if err != nil {
		return nil, err
	}
	if p.Action == "rebind" {
		fetchCtx, cancel := context.WithTimeout(ctx, config.DefaultBranchSyncRemoteTimeout)
		err := git.FetchRemoteRef(fetchCtx, gate, pushURL, "refs/heads/"+destination, live)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("import destination head: %w", err)
		}
	}
	wt := worktrees.RecordedDir(m.paths, run.WorktreePath(), run.RepoID, run.ID)
	// Any existing managed worktree must be clean both before and after stop.
	inspectHead := func() (string, error) {
		if _, e := os.Stat(wt); e == nil {
			dirty, e := git.Run(ctx, wt, "status", "--porcelain", "--untracked-files=all")
			if e != nil {
				return "", e
			}
			if dirty != "" {
				return "", fmt.Errorf("managed worktree has unpreserved changes; refusing custody handoff")
			}
			return git.HeadSHA(ctx, wt)
		} else if !os.IsNotExist(e) {
			return "", e
		}
		head, e := git.Run(ctx, gate, "rev-parse", "--verify", run.HeadSHA+"^{commit}")
		if e != nil {
			return "", fmt.Errorf("recorded head is unavailable: %w", e)
		}
		return head, nil
	}
	m.custodyCleanupMu.Lock()
	cleanupLocked := true
	defer func() {
		if cleanupLocked {
			m.custodyCleanupMu.Unlock()
		}
	}()
	head, err := inspectHead()
	if err != nil {
		return nil, err
	}
	checkHead := func(head string) error {
		if p.Action != "rebind" && head != live {
			return fmt.Errorf("unpublished run head would be abandoned; release requires the exact published head")
		}
		if p.Action == "rebind" {
			if _, e := git.Run(ctx, gate, "merge-base", "--is-ancestor", live, head); e != nil {
				return fmt.Errorf("rebind requires destination history contained in the preserved run head")
			}
		}
		return nil
	}
	if err := checkHead(head); err != nil {
		return nil, err
	}
	preserve := func(head string) error {
		verified, err := git.Run(ctx, gate, "rev-parse", "--verify", head+"^{commit}")
		if err != nil || verified != head {
			return fmt.Errorf("head is not an exact commit")
		}
		return custody.PreserveRecoveryAnchor(ctx, gate, "refs/no-mistakes/custody/"+run.ID+"/"+head, head)
	}
	preserveSnapshot := func(head string) error {
		heads := []string{head, run.HeadSHA}
		if run.LastPushedSHA != nil {
			heads = append(heads, *run.LastPushedSHA)
		}
		for _, verified := range heads {
			if err := preserve(verified); err != nil {
				return fmt.Errorf("preserve handoff head %s: %w", verified, err)
			}
		}
		return nil
	}
	if err := preserveSnapshot(head); err != nil {
		return nil, err
	}
	gateBefore, gateExists, err := git.ExactRefTarget(ctx, gate, "refs/heads/"+p.Branch)
	if err != nil || !gateExists {
		return nil, fmt.Errorf("source gate ref cannot be verified: %v", err)
	}
	if err := preserve(gateBefore); err != nil {
		return nil, err
	}
	if err := m.db.HoldCustodyWorktree(run.ID); err != nil {
		return nil, err
	}
	m.custodyCleanupMu.Unlock()
	cleanupLocked = false
	m.mu.Lock()
	cancel := m.cancels[run.ID]
	done := m.dones[run.ID]
	m.mu.Unlock()
	if cancel != nil {
		if done == nil {
			return nil, fmt.Errorf("executor has no quiescence proof")
		}
		cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("executor has not stopped; custody remains held")
		}
	} else if run.Status == types.RunRunning || run.Status == types.RunPending {
		return nil, fmt.Errorf("active record has no owned executor; reconcile daemon recovery first")
	}
	run, err = m.db.GetRun(run.ID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status == types.RunRunning || run.Status == types.RunPending {
		return nil, fmt.Errorf("run did not become terminal")
	}
	head, err = inspectHead()
	if err != nil {
		return nil, err
	}
	if err := preserveSnapshot(head); err != nil {
		return nil, err
	}
	if err := checkHead(head); err != nil {
		return nil, err
	}
	again, againBase, err := verify()
	if err != nil {
		return nil, err
	}
	if again != live || againBase != base {
		return nil, fmt.Errorf("destination changed during quiescence")
	}
	currentRepo, err := m.db.GetRepo(repo.ID)
	if err != nil {
		return nil, err
	}
	if currentRepo == nil || currentRepo.PushURL() != repo.PushURL() {
		return nil, fmt.Errorf("push target changed")
	}
	ref := "refs/heads/" + p.Branch
	old, exists, err := git.ExactRefTarget(ctx, gate, ref)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("source gate branch disappeared")
	}
	if err := preserve(old); err != nil {
		return nil, err
	}
	if old != gateBefore {
		return nil, fmt.Errorf("source gate changed during quiescence; both heads are preserved")
	}
	stack, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, err
	}
	var settled []*db.Run
	service := &branchsync.Service{DB: m.db, Repo: repo, GateDir: gate}
	for _, candidate := range service.OutstandingCustodyRuns(ctx, p.Branch, stack) {
		if candidate.ID == run.ID {
			continue
		}
		candidateDir := worktrees.RecordedDir(m.paths, candidate.WorktreePath(), candidate.RepoID, candidate.ID)
		if _, err := os.Stat(candidateDir); err == nil {
			dirty, err := git.Run(ctx, candidateDir, "status", "--porcelain", "--untracked-files=all")
			if err != nil || dirty != "" {
				return nil, fmt.Errorf("run %s has unpreserved worktree changes", candidate.ID)
			}
			actual, err := git.HeadSHA(ctx, candidateDir)
			if err != nil || actual != candidate.HeadSHA {
				return nil, fmt.Errorf("run %s worktree head differs from its custody record", candidate.ID)
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if _, err := git.Run(ctx, gate, "merge-base", "--is-ancestor", candidate.HeadSHA, head); err != nil {
			return nil, fmt.Errorf("run %s has unpublished work outside the handoff head", candidate.ID)
		}
		if err := custody.PreserveRecoveryHead(ctx, gate, candidate.ID, candidate.HeadSHA); err != nil {
			return nil, err
		}
		settled = append(settled, candidate)
	}
	if _, err := git.Run(ctx, gate, "update-ref", "--no-deref", ref, head, old); err != nil {
		return nil, err
	}
	var binding *db.PublicationBinding
	if p.Action == "rebind" {
		binding = &db.PublicationBinding{Branch: destination, PRURL: expectedPR, Target: branchsync.TargetFingerprint(repo.PushURL()), BaseBranch: base, SourceRun: run.ID}
	}
	if err := m.db.FinishCustody(run, binding, settled...); err != nil {
		_, rollback := git.Run(context.WithoutCancel(ctx), gate, "update-ref", "--no-deref", ref, old, head)
		return nil, fmt.Errorf("record custody: %w (gate rollback: %v)", err, rollback)
	}
	outcome := "custody-returned"
	if binding != nil {
		outcome = "rerun-required"
	}
	next := ""
	if binding != nil {
		next = "no-mistakes custody validate --head " + head
	}
	return &ipc.CustodyResult{RunID: run.ID, Outcome: outcome, Head: head, PRURL: expectedPR, Destination: destination, NextCommand: next}, nil
}

// validateCustodySuccessor is explicit selection of the archived managed head,
// not an exemption to rerun's clean-caller-head check. It never moves local HEAD.
func (m *RunManager) validateCustodySuccessor(ctx context.Context, p ipc.CustodyParams) (*ipc.CustodyResult, error) {
	var result *ipc.CustodyResult
	_, err := m.withBranchLock(p.RepoID, p.Branch, func() (string, error) {
		runs, err := m.db.GetRunsByRepo(p.RepoID)
		if err != nil {
			return "", err
		}
		var run *db.Run
		for _, r := range runs {
			if r.Branch == p.Branch {
				run = r
				break
			}
		}
		if run == nil || run.ID != p.ExpectedRun || !run.Status.Terminal() || run.Publication == nil || run.Publication.SourceRun != run.ID {
			return "", fmt.Errorf("no pending rebind for this branch; inspect status")
		}
		gate := m.paths.RepoDir(p.RepoID)
		ref := "refs/no-mistakes/custody/" + run.ID + "/" + p.Head
		archived, exists, err := git.ExactRefTarget(ctx, gate, ref)
		if err != nil || !exists || archived != p.Head || p.Head == "" {
			return "", fmt.Errorf("requested head is not this handoff's exact archive")
		}
		head, exists, err := git.ExactRefTarget(ctx, gate, "refs/heads/"+p.Branch)
		if err != nil || !exists || head != p.Head {
			return "", fmt.Errorf("source gate changed after handoff")
		}
		repo, err := m.db.GetRepo(p.RepoID)
		if err != nil {
			return "", err
		}
		if repo == nil {
			return "", fmt.Errorf("repository disappeared")
		}
		intent := ""
		source := ""
		if run.Intent != nil && run.IntentSource != nil && db.IsAuthoritativeRunIntentSource(*run.IntentSource) {
			intent = *run.Intent
			source = db.RunIntentSourceRerun
		}
		baseOverride := ""
		if run.PRBaseBranch != nil {
			baseOverride = *run.PRBaseBranch
		}
		id, err := m.startRunWithIntentSourceLocked(ctx, repo, p.Branch, p.Head, run.BaseSHA, "rerun", nil, intent, source, "", "", "", baseOverride, run.OmitIntent, run.Publication.PRURL, "", nil)
		if err != nil {
			return "", err
		}
		result = &ipc.CustodyResult{RunID: id, Outcome: "validation-started", Head: p.Head, PRURL: run.Publication.PRURL, Destination: run.Publication.Branch, NextCommand: "no-mistakes axi run"}
		return id, nil
	})
	return result, err
}
