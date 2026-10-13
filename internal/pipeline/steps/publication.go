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
func verifyPublicationDestination(sctx *pipeline.StepContext) (*scm.PR, error) {
	p := sctx.Run.Publication
	if p == nil {
		return nil, nil
	}
	if branchsync.TargetFingerprint(sctx.Repo.PushURL()) != p.Target || branchsync.TargetFingerprint(resolvePushURL(sctx)) != p.Target {
		return nil, fmt.Errorf("rebound publication target changed")
	}
	host, err := PublicationHost(sctx)
	if err != nil {
		return nil, err
	}
	pr, err := host.FindPR(sctx.Ctx, p.Branch, "")
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.URL != p.PRURL {
		return nil, fmt.Errorf("rebound destination PR is absent or replaced")
	}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil {
		return nil, err
	}
	if state != scm.PRStateOpen {
		return nil, fmt.Errorf("rebound destination PR is not open")
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, config.DefaultBranchSyncRemoteTimeout)
	defer cancel()
	head, err := git.LsRemote(ctx, sctx.WorkDir, resolvePushURL(sctx), "refs/heads/"+p.Branch)
	if err != nil {
		return nil, err
	}
	if err := scm.VerifyPRHeadIdentity(ctx, host, pr, sctx.Repo.PushURL(), p.Branch, head); err != nil {
		return nil, err
	}
	return pr, nil
}

func resolvePublicationBaseBranch(sctx *pipeline.StepContext) (string, error) {
	if sctx.Run.Publication == nil || runPRBaseBranch(sctx) != "" {
		return effectivePRBaseBranch(sctx), nil
	}
	pr, err := verifyPublicationDestination(sctx)
	if err != nil {
		return "", err
	}
	if pr.BaseBranch == "" {
		return "", fmt.Errorf("rebound PR has no live base branch")
	}
	return ValidateRunPRBaseBranchName(pr.BaseBranch)
}
