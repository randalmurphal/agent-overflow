package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// gitFailureCause is what a failing hook or forge CLI prints: a path and a
// token-like string, which belong in the host log and never in an outcome.
const gitFailureCause = "/home/alice/.ssh/id_ed25519 token=ghp_0123456789abcdefSECRET"

// failHook installs the git hook name in workspace to print the cause and
// fail.
func failHook(t *testing.T, workspace, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	hooks := strings.TrimSpace(gitText(t, workspace, "rev-parse", "--git-path", "hooks"))
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(workspace, hooks)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\necho '"+gitFailureCause+"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A failed commit, push and create PR each answer their outcome with the
// write's public code and message, never its raw cause, and a repeat of
// the attempt id answers with the error the first caller got.
func TestAFailedGitAttemptsOutcomeIsItsPublicMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action string
		setup  func(t *testing.T, app *App, workspace, remote string)
		write  func(app *App, ref WorkspaceRef, id string) (gitops.GitActionResult, error)
	}{
		{gitActionCommit, func(t *testing.T, _ *App, workspace, _ string) {
			failHook(t, workspace, "pre-commit")
			writeWorkspaceFile(t, workspace, "README.txt", "hello\nrefused\n")
		}, func(app *App, ref WorkspaceRef, id string) (gitops.GitActionResult, error) {
			return app.GitCommit(ref, "refused", "", CommitSelection{All: true}, id)
		}},
		{gitActionPush, func(t *testing.T, _ *App, workspace, remote string) {
			testutil.RunGit(t, workspace, "remote", "add", "origin", remote)
			failHook(t, workspace, "pre-push")
		}, func(app *App, ref WorkspaceRef, id string) (gitops.GitActionResult, error) {
			return app.GitPush(ref, id)
		}},
		{gitActionPR, func(t *testing.T, app *App, workspace, _ string) {
			testutil.RunGit(t, workspace, "remote", "add", "origin", "https://github.com/test/test.git")
			installMockGh(t, app, "", gitFailureCause, 1)
		}, func(app *App, ref WorkspaceRef, id string) (gitops.GitActionResult, error) {
			return app.GitCreatePR(context.Background(), ref, "Refused PR", "", false, id)
		}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			app, ref, workspace, remote := gitActionTestSetup(t)
			tc.setup(t, app, workspace, remote)
			_, first := tc.write(app, ref, "attempt-1")
			if first == nil {
				t.Fatalf("the %s succeeded", tc.action)
			}
			outcome, err := app.GetGitActionOutcome(context.Background(), ref, tc.action, "attempt-1")
			if err != nil || outcome.State != "failed" || outcome.Code != gitActionFailedCode || outcome.Message == "" || outcome.Result != nil {
				t.Fatalf("outcome = %+v, %v; want failed with code %s and a message", outcome, err, gitActionFailedCode)
			}
			if strings.Contains(outcome.Message, "id_ed25519") || strings.Contains(outcome.Message, "ghp_") {
				t.Fatalf("the outcome carries the raw cause: %q", outcome.Message)
			}
			_, again := tc.write(app, ref, "attempt-1")
			if again == nil || again.Error() != first.Error() {
				t.Fatalf("the repeated attempt answered %v; want the first caller's %v", again, first)
			}
		})
	}
}

// A failure with a public message of its own answers with that message
// and code, as the first caller got it.
func TestAFailedGitAttemptKeepsItsOwnPublicMessage(t *testing.T) {
	t.Parallel()
	app, ref, _, _ := gitActionTestSetup(t)
	elsewhere := WorkspaceRef{ProjectID: ref.ProjectID, WorkspacePath: filepath.Join(t.TempDir(), "not-a-workspace")}
	if _, err := app.GitPush(elsewhere, "attempt-1"); err == nil {
		t.Fatal("a push to a path that is no workspace succeeded")
	}
	outcome, err := app.GetGitActionOutcome(context.Background(), elsewhere, gitActionPush, "attempt-1")
	if err != nil || outcome.State != "failed" || outcome.Code != "workspace_not_registered" || strings.Contains(outcome.Message, elsewhere.WorkspacePath) {
		t.Fatalf("outcome = %+v, %v; want the workspace refusal's own code and message", outcome, err)
	}
}
