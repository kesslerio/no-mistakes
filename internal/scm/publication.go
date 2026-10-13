package scm

import (
	"context"
	"fmt"
	"strings"
)

type PRHeadIdentity struct {
	RepositoryURL string
	Branch        string
	SHA           string
}

type PRHeadIdentityReader interface {
	ReadPRHeadIdentity(context.Context, *PR) (PRHeadIdentity, error)
}

func VerifyPRHeadIdentity(ctx context.Context, host Host, pr *PR, target, branch, sha string) error {
	reader, ok := host.(PRHeadIdentityReader)
	if !ok {
		return fmt.Errorf("provider cannot verify the PR head repository and branch")
	}
	identity, err := reader.ReadPRHeadIdentity(ctx, pr)
	if err != nil {
		return err
	}
	path := strings.TrimSuffix(RepoPath(target), ".git")
	actual := strings.TrimSuffix(RepoPath(identity.RepositoryURL), ".git")
	if identity.RepositoryURL == "" || path == "" || actual != path || !strings.EqualFold(ResolveHost(ctx, identity.RepositoryURL), ResolveHost(ctx, target)) || identity.Branch != branch || sha == "" || identity.SHA != sha {
		return fmt.Errorf("PR head repository, branch, and commit do not match the publication destination")
	}
	return nil
}
