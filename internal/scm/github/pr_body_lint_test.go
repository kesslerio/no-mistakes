package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRBodyLintOffDoesNotInvokeTheBinary(t *testing.T) {
	t.Parallel()

	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "pr-body-lint" {
			t.Errorf("pr-body-lint invoked while the check is off")
		}
		return githubHelperCmd(ctx, githubTestResponse{stdout: "https://github.com/test/repo/pull/1\n"})
	}, nil, "", "test/repo")
	if host.lintPRBodies {
		t.Fatal("New under go test enabled pr-body-lint")
	}
	if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestPRBodyLintBeforePublish(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n![shot](https://github.com/user-attachments/assets/abc)\n</details>\n"
	const lintFailure = "Images in this GitHub text will not display: images directly under a </details> line render as literal text: https://github.com/user-attachments/assets/abc.\nLeave a blank line between that tag line and the image."

	cases := []struct {
		name         string
		update       bool
		mode         string // pass, fail, absent
		repo         string
		wantLintVerb string
	}{
		{name: "create pass", mode: "pass", repo: "test/repo", wantLintVerb: "create"},
		{name: "create fail", mode: "fail", repo: "test/repo", wantLintVerb: "create"},
		{name: "create absent", mode: "absent", repo: "test/repo", wantLintVerb: "create"},
		{name: "update pass", update: true, mode: "pass", repo: "test/repo", wantLintVerb: "edit"},
		{name: "update fail", update: true, mode: "fail", repo: "test/repo", wantLintVerb: "edit"},
		{name: "update absent", update: true, mode: "absent", repo: "test/repo", wantLintVerb: "edit"},
		// gh pr create against kunchenguid is an agent-bypass block. The
		// pipeline is the authorized publisher, so the lint command uses edit
		// while gh itself still creates.
		{name: "create kunchenguid", mode: "pass", repo: "kunchenguid/no-mistakes", wantLintVerb: "edit"},
		{name: "create unknown slug", mode: "pass", repo: "", wantLintVerb: "edit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			var notes []string
			var lintArgs []string
			var lintBody string
			var ghArgs []string

			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name == "pr-body-lint" {
					lintArgs = append([]string(nil), args...)
					if tc.mode == "absent" {
						cmd := exec.CommandContext(ctx, name)
						cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
						return cmd
					}
					lintBody = readLintBody(t, args)
					stderr := ""
					code := 0
					if tc.mode == "fail" {
						stderr = lintFailure
						code = 2
					}
					return githubHelperCmd(ctx, githubTestResponse{stderr: stderr, code: code})
				}
				ghArgs = append([]string{name}, args...)
				return githubHelperCmd(ctx, githubTestResponse{
					stdout:    "https://github.com/test/repo/pull/42\n",
					wantStdin: body,
				})
			}, nil, "", tc.repo)
			host.lintPRBodies = true
			host.workDir = workDir
			host.stepLog = func(line string) { notes = append(notes, line) }

			var publishErr error
			if tc.update {
				_, publishErr = host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{
					Title: "fix: lint body",
					Body:  body,
				})
			} else {
				_, publishErr = host.CreatePR(context.Background(), "feature", "main", scm.PRContent{
					Title: "fix: lint body",
					Body:  body,
				})
			}

			if len(lintArgs) == 0 || lintArgs[0] != "--command" || !containsArg(lintArgs, "--cwd") {
				t.Fatalf("pr-body-lint args = %q, want --command and --cwd", lintArgs)
			}
			if cwd := flagValue(lintArgs, "--cwd"); cwd != workDir {
				t.Fatalf("lint --cwd = %q, want %q", cwd, workDir)
			}
			command := flagValue(lintArgs, "--command")
			wantCommandPrefix := "gh pr " + tc.wantLintVerb + " "
			if !strings.HasPrefix(command, wantCommandPrefix) && command != "gh pr "+tc.wantLintVerb {
				t.Fatalf("lint command = %q, want prefix %q", command, wantCommandPrefix)
			}
			if tc.wantLintVerb == "edit" && strings.Contains(command, "gh pr create") {
				t.Fatalf("lint command used gh pr create: %q", command)
			}
			if tc.repo == "" {
				if strings.Contains(command, "--repo") {
					t.Fatalf("lint command included --repo for an unknown slug: %q", command)
				}
			} else if !strings.Contains(command, "--repo "+tc.repo) {
				t.Fatalf("lint command = %q, want --repo %s", command, tc.repo)
			}

			switch tc.mode {
			case "pass":
				if publishErr != nil {
					t.Fatalf("publish error = %v, want nil", publishErr)
				}
				if lintBody != body {
					t.Fatalf("lint saw body %q, want %q", lintBody, body)
				}
				if len(notes) != 0 {
					t.Fatalf("notes = %q, want none when the lint runs", notes)
				}
				assertGHPublished(t, tc.update, ghArgs)
			case "fail":
				if publishErr == nil {
					t.Fatal("publish error = nil, want the lint output")
				}
				if publishErr.Error() != lintFailure {
					t.Fatalf("publish error = %q, want the lint's own output", publishErr.Error())
				}
				if lintBody != body {
					t.Fatalf("lint saw body %q, want %q", lintBody, body)
				}
				if len(ghArgs) != 0 {
					t.Fatalf("gh was invoked after a lint failure: %q", ghArgs)
				}
				if len(notes) != 0 {
					t.Fatalf("notes = %q, want none when the lint fails", notes)
				}
			case "absent":
				if publishErr != nil {
					t.Fatalf("publish error = %v, want nil when pr-body-lint is absent", publishErr)
				}
				if len(notes) != 1 || notes[0] != prBodyLintSkipNote {
					t.Fatalf("notes = %q, want one skip line", notes)
				}
				if strings.Contains(notes[0], "\n") {
					t.Fatalf("skip note must be one line, got %q", notes[0])
				}
				assertGHPublished(t, tc.update, ghArgs)
			default:
				t.Fatalf("unknown mode %q", tc.mode)
			}
		})
	}
}

func assertGHPublished(t *testing.T, update bool, ghArgs []string) {
	t.Helper()
	if len(ghArgs) == 0 || ghArgs[0] != "gh" {
		t.Fatalf("gh args = %q, want a gh invocation", ghArgs)
	}
	joined := strings.Join(ghArgs, " ")
	if update {
		if !strings.Contains(joined, "pr edit 42") {
			t.Fatalf("gh args = %q, want pr edit 42", ghArgs)
		}
	} else if !strings.Contains(joined, "pr create") {
		t.Fatalf("gh args = %q, want pr create", ghArgs)
	}
	if !strings.Contains(joined, "--body-file -") {
		t.Fatalf("gh must still publish the body on stdin, got %q", ghArgs)
	}
}

func readLintBody(t *testing.T, args []string) string {
	t.Helper()
	command := flagValue(args, "--command")
	path := bodyFileFromCommand(command)
	if path == "" {
		t.Fatalf("lint command %q has no --body-file", command)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lint body file %s: %v", path, err)
	}
	return string(data)
}

func bodyFileFromCommand(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--body-file" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], "'")
		}
	}
	return ""
}

func flagValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func githubHelperCmd(ctx context.Context, response githubTestResponse) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", "pr-body-lint")
	cmd.Env = append(os.Environ(),
		"GITHUB_TEST_HELPER=1",
		"GITHUB_TEST_STDOUT="+response.stdout,
		"GITHUB_TEST_STDERR="+response.stderr,
		"GITHUB_TEST_WANT_STDIN="+response.wantStdin,
		fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
	)
	return cmd
}
