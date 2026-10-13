package steps

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// PublicationHost shares provider routing and credentials with pipeline writes.
func PublicationHost(sctx *pipeline.StepContext) (scm.Host, error) {
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, fmt.Errorf("publication host unavailable: %s", reason)
	}
	return host, nil
}
func verifyPublicationDestination(sctx *pipeline.StepContext) error {
	p := sctx.Run.Publication
	if p == nil {
		return nil
	}
	if branchsync.TargetFingerprint(sctx.Repo.PushURL()) != p.Target || branchsync.TargetFingerprint(resolvePushURL(sctx)) != p.Target {
		return fmt.Errorf("rebound publication target changed")
	}
	host, err := PublicationHost(sctx)
	if err != nil {
		return err
	}
	pr, err := host.FindPR(sctx.Ctx, p.Branch, "")
	if err != nil {
		return err
	}
	if pr == nil || pr.URL != p.PRURL {
		return fmt.Errorf("rebound destination PR is absent or replaced")
	}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil {
		return err
	}
	if state != scm.PRStateOpen {
		return fmt.Errorf("rebound destination PR is not open")
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, config.DefaultBranchSyncRemoteTimeout)
	defer cancel()
	head, err := git.LsRemote(ctx, sctx.WorkDir, resolvePushURL(sctx), "refs/heads/"+p.Branch)
	if err != nil {
		return err
	}
	return scm.VerifyPRHeadIdentity(ctx, host, pr, sctx.Repo.PushURL(), p.Branch, head)
}
