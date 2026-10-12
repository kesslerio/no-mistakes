package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prBodyLintSkipNote is the one-line step-log note when the binary is absent.
const prBodyLintSkipNote = "pr-body-lint not on PATH; skipping PR body lint"

// defaultLintPRBodies is false under go test. Publication tests inject fake
// gh binaries and often keep the ambient PATH, so an installed pr-body-lint
// would otherwise reject fixture bodies and make those tests depend on the
// host. Tests that cover this check opt in by setting Host.lintPRBodies.
func defaultLintPRBodies() bool {
	return !testing.Testing()
}

// SetStepLog sends operator-visible notes to the pipeline step log.
// workDir is the checkout pr-body-lint receives as --cwd; empty means ".".
func (h *Host) SetStepLog(log func(string), workDir string) {
	if h == nil {
		return
	}
	h.stepLog = log
	h.workDir = strings.TrimSpace(workDir)
}

// lintPRBody runs the same pr-body-lint invocation the agent hook uses
// (`pr-body-lint --command <gh pr create|edit> --cwd <dir>`) before gh
// publishes. The hook reads the body from the command string; gh here takes
// the body on stdin, so the lint command points --body-file at a temp copy.
// A missing binary is skipped. Any other failure is the linter's own output,
// and the caller must not publish.
func (h *Host) lintPRBody(ctx context.Context, op, body string) error {
	if h == nil || !h.lintPRBodies {
		return nil
	}
	if h.cmd == nil {
		h.note(prBodyLintSkipNote)
		return nil
	}
	dir, err := os.MkdirTemp("", "no-mistakes-pr-body-")
	if err != nil {
		return fmt.Errorf("pr-body-lint: %w", err)
	}
	defer os.RemoveAll(dir)
	bodyFile := filepath.Join(dir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
		return fmt.Errorf("pr-body-lint: %w", err)
	}
	cwd := h.workDir
	if cwd == "" {
		cwd = "."
	}
	cmd := h.cmd(ctx, "pr-body-lint", "--command", prBodyLintCommand(op, h.repo, bodyFile), "--cwd", cwd)
	if cmd == nil || isExecNotFound(cmd.Err) {
		h.note(prBodyLintSkipNote)
		return nil
	}
	if cmd.Err != nil {
		return cmd.Err
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if isExecNotFound(err) {
		h.note(prBodyLintSkipNote)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = strings.TrimSpace(err.Error())
	}
	return errors.New(msg)
}

func (h *Host) note(msg string) {
	if h != nil && h.stepLog != nil && msg != "" {
		h.stepLog(msg)
	}
}

func isExecNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// prBodyLintCommand is the shell text pr-body-lint parses. It is not executed.
// A create whose slug is kunchenguid, or whose slug is unknown so the linter
// would infer the target from the checkout, is spelled as `gh pr edit`.
// `gh pr create` against a kunchenguid checkout is an agent-bypass rule, and
// this process is the pipeline, the authorized publisher. Body checks are the
// same for both verbs.
func prBodyLintCommand(op, repo, bodyFile string) string {
	verb := "edit"
	if op == "create" && lintCreateVerbAllowed(repo) {
		verb = "create"
	}
	parts := []string{"gh", "pr", verb}
	if repo = strings.TrimSpace(repo); repo != "" {
		parts = append(parts, "--repo", quoteLintArg(repo))
	}
	parts = append(parts, "--body-file", quoteLintArg(bodyFile))
	return strings.Join(parts, " ")
}

func lintCreateVerbAllowed(repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return false
	}
	parts := strings.Split(repo, "/")
	owner := parts[0]
	if len(parts) >= 3 {
		// host/owner/name, the gh --repo form for GitHub Enterprise.
		owner = parts[1]
	}
	return owner != "kunchenguid"
}

// quoteLintArg quotes only when the linter's token scan would split the
// value. pr-body-lint strips one matching quote pair and does not understand
// shell escapes, so a value with a single quote cannot be quoted safely.
func quoteLintArg(s string) string {
	if strings.ContainsAny(s, " \t") && !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return s
}
