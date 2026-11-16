package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// gitFailureCause is what a failing hook or forge CLI prints: the reason
// the person needs to see, whether the answer came at once or was lost.
const gitFailureCause = "pre-push: lint found 3 problems in main.go"

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

// appDispatch calls app's bound methods the way a client does, through
// the transport's dispatcher.
type appDispatch struct {
	t *testing.T
	d *transport.Dispatcher
}

func newAppDispatch(t *testing.T, app *App) appDispatch {
	t.Helper()
	d := transport.NewDispatcher()
	if _, err := d.Register(app, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
		t.Fatal(err)
	}
	return appDispatch{t: t, d: d}
}

// call invokes method with args and returns its result or its error frame.
func (a appDispatch) call(method string, args ...any) (json.RawMessage, *transport.FrameError) {
	a.t.Helper()
	m, ok := a.d.LookupName(method)
	if !ok {
		a.t.Fatalf("no bound method %s", method)
	}
	params := make([]json.RawMessage, len(args))
	for i, arg := range args {
		raw, err := json.Marshal(arg)
		if err != nil {
			a.t.Fatal(err)
		}
		params[i] = raw
	}
	return a.d.Invoke(context.Background(), m, params)
}

// failed calls a write that must fail and returns what its caller was told.
func (a appDispatch) failed(method string, args ...any) *transport.FrameError {
	a.t.Helper()
	raw, frame := a.call(method, args...)
	if frame == nil {
		a.t.Fatalf("%s succeeded: %s", method, raw)
	}
	return frame
}

// outcome calls an outcome method and decodes its answer into out.
func (a appDispatch) outcome(out any, method string, args ...any) {
	a.t.Helper()
	raw, frame := a.call(method, args...)
	if frame != nil {
		a.t.Fatalf("%s: %+v", method, frame)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		a.t.Fatal(err)
	}
}

// sameAnswer fails unless a failed outcome says exactly what the write's
// first caller was told.
func sameAnswer(t *testing.T, state, code, message string, chain []string, first *transport.FrameError) {
	t.Helper()
	var firstChain []string
	if first.Detail != nil {
		firstChain = first.Detail.Chain
	}
	if state != "failed" || code != first.Code || message != first.Message || !slices.Equal(chain, firstChain) {
		t.Fatalf("outcome = %s %q %q %q\nfirst caller = %q %q %q", state, code, message, chain, first.Code, first.Message, firstChain)
	}
}

// A failed commit, push and create PR each answer their outcome with
// exactly what the first caller was told, the hook's or the forge CLI's
// reason included, and a repeat of the attempt id is told the same.
func TestAFailedGitAttemptsOutcomeIsWhatItsCallerWasTold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action, method string
		setup          func(t *testing.T, app *App, workspace, remote string)
		args           func(ref WorkspaceRef) []any
	}{
		{gitActionCommit, "GitCommit", func(t *testing.T, _ *App, workspace, _ string) {
			failHook(t, workspace, "pre-commit")
			writeWorkspaceFile(t, workspace, "README.txt", "hello\nrefused\n")
		}, func(ref WorkspaceRef) []any {
			return []any{ref, "refused", "", CommitSelection{All: true}, "attempt-1"}
		}},
		{gitActionPush, "GitPush", func(t *testing.T, _ *App, workspace, remote string) {
			testutil.RunGit(t, workspace, "remote", "add", "origin", remote)
			failHook(t, workspace, "pre-push")
		}, func(ref WorkspaceRef) []any { return []any{ref, "attempt-1"} }},
		{gitActionPR, "GitCreatePR", func(t *testing.T, app *App, workspace, _ string) {
			testutil.RunGit(t, workspace, "remote", "add", "origin", "https://github.com/test/test.git")
			installMockGh(t, app, "", gitFailureCause, 1)
		}, func(ref WorkspaceRef) []any { return []any{ref, "Refused PR", "", false, "attempt-1"} }},
	} {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			app, ref, workspace, remote := gitActionTestSetup(t)
			tc.setup(t, app, workspace, remote)
			client := newAppDispatch(t, app)
			first := client.failed(tc.method, tc.args(ref)...)
			if !strings.Contains(first.Message, gitFailureCause) {
				t.Fatalf("the first caller was not told the reason: %+v", first)
			}
			var outcome GitActionOutcome
			client.outcome(&outcome, "GetGitActionOutcome", ref, tc.action, "attempt-1")
			sameAnswer(t, outcome.State, outcome.Code, outcome.Message, outcome.Chain, first)
			again := client.failed(tc.method, tc.args(ref)...)
			if again.Code != first.Code || again.Message != first.Message {
				t.Fatalf("the repeated attempt was told %q %q; the first caller %q %q", again.Code, again.Message, first.Code, first.Message)
			}
		})
	}
}

// A failure with a public message of its own answers its outcome with that
// code and message alone, as its first caller was told.
func TestAFailedGitAttemptKeepsItsOwnPublicMessage(t *testing.T) {
	t.Parallel()
	app, ref, _, _ := gitActionTestSetup(t)
	client := newAppDispatch(t, app)
	elsewhere := WorkspaceRef{ProjectID: ref.ProjectID, WorkspacePath: filepath.Join(t.TempDir(), "not-a-workspace")}
	first := client.failed("GitPush", elsewhere, "attempt-1")
	if first.Code != "workspace_not_registered" || strings.Contains(first.Message, elsewhere.WorkspacePath) {
		t.Fatalf("first caller = %+v; want the workspace refusal's own sentence", first)
	}
	var outcome GitActionOutcome
	client.outcome(&outcome, "GetGitActionOutcome", elsewhere, gitActionPush, "attempt-1")
	sameAnswer(t, outcome.State, outcome.Code, outcome.Message, outcome.Chain, first)
}

// A review submit the forge refused answers its outcome with exactly what
// its first caller was told.
func TestAFailedReviewSubmitsOutcomeIsWhatItsCallerWasTold(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	draft := c.draft(t, false, 3, "Guard this.")
	if err := c.engine.SetRefusal(forgefake.Refusal{Route: publishRoute["github"], Status: http.StatusUnprocessableEntity, Times: 1}); err != nil {
		t.Fatal(err)
	}
	review := c.request(false, draft)
	review.AttemptID = "attempt-422"
	client := newAppDispatch(t, c.app)
	first := client.failed("SubmitPRReview", c.pr, review)
	var outcome PRReviewSubmitOutcome
	client.outcome(&outcome, "GetPRReviewSubmitOutcome", c.pr, "attempt-422")
	sameAnswer(t, outcome.State, outcome.Code, outcome.Message, outcome.Chain, first)
}
