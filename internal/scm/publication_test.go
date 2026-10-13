package scm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type publicationIdentityHost struct {
	Host
	identity PRHeadIdentity
}

func (h *publicationIdentityHost) ReadPRHeadIdentity(context.Context, *PR) (PRHeadIdentity, error) {
	return h.identity, nil
}

func TestVerifyPRHeadIdentityResolvesSSHAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH executable fixture requires a POSIX shell")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf 'hostname github.com\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, test := range []struct {
		name, repo, branch, sha string
		allowed                 bool
	}{
		{"alias", "https://github.com/team/repo.git", "destination", "head", true},
		{"wrong-host", "https://other.example/team/repo.git", "destination", "head", false},
		{"wrong-repo", "https://github.com/fork/repo.git", "destination", "head", false},
		{"wrong-branch", "https://github.com/team/repo.git", "source", "head", false},
		{"wrong-sha", "https://github.com/team/repo.git", "destination", "other", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &publicationIdentityHost{identity: PRHeadIdentity{RepositoryURL: test.repo, Branch: test.branch, SHA: test.sha}}
			err := VerifyPRHeadIdentity(context.Background(), host, &PR{Number: "42"}, "git@github-work:team/repo.git", "destination", "head")
			if (err == nil) != test.allowed {
				t.Fatalf("verification = %v, allowed = %v", err, test.allowed)
			}
		})
	}
}
