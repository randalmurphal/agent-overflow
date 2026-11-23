package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// heldGate is a process the test holds open: it counts its runs in a file
// and waits until release is called, as a slow hook or forge CLI would.
type heldGate struct {
	count, gate string
	released    bool
	t           *testing.T
}

func newHeldGate(t *testing.T) *heldGate {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the held process is a POSIX shell script")
	}
	dir := t.TempDir()
	g := &heldGate{count: filepath.Join(dir, "runs"), gate: filepath.Join(dir, "release"), t: t}
	t.Cleanup(g.release)
	return g
}

// script is the shell that counts a run, then waits for the release.
func (g *heldGate) script() string {
	return "echo run >> '" + g.count + "'\nwhile [ ! -f '" + g.gate + "' ]; do sleep 0.02; done\n"
}

func (g *heldGate) release() {
	if g.released {
		return
	}
	g.released = true
	if err := os.WriteFile(g.gate, nil, 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *heldGate) runs() int {
	data, err := os.ReadFile(g.count)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		g.t.Fatal(err)
	}
	return strings.Count(string(data), "run")
}

func (g *heldGate) waitRunning() {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for g.runs() == 0 {
		if time.Now().After(deadline) {
			g.t.Fatal("the held process never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdHook installs the git hook name in workspace as a held gate.
func holdHook(t *testing.T, workspace, name string) *heldGate {
	t.Helper()
	g := newHeldGate(t)
	hooks := strings.TrimSpace(gitText(t, workspace, "rev-parse", "--git-path", "hooks"))
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(workspace, hooks)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	mockexec.WriteFile(t, filepath.Join(hooks, name), []byte("#!/bin/sh\n"+g.script()), 0o755)
	return g
}

type gitAnswer struct {
	result gitops.GitActionResult
	err    error
}

// heldAttempt checks the attempt contract for one git write held open by
// gate: the outcome waits for the running write and reports it, a repeat of
// the attempt waits for and answers with the first one's result, and the
// write ran once.
func heldAttempt(t *testing.T, app *App, ref WorkspaceRef, action string, gate *heldGate, write func(attemptID string) (gitops.GitActionResult, error)) gitops.GitActionResult {
	t.Helper()
	ctx := context.Background()
	if outcome, err := app.GetGitActionOutcome(ctx, ref, action, "attempt-1"); err != nil || outcome.State != "unknown" {
		t.Fatalf("outcome before the %s arrived = %+v, %v", action, outcome, err)
	}
	first := make(chan gitAnswer, 1)
	go func() {
		result, err := write("attempt-1")
		first <- gitAnswer{result, err}
	}()
	gate.waitRunning()

	// A caller that leaves stops only its own wait.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := app.GetGitActionOutcome(gone, ref, action, "attempt-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome for a caller that left = %v, want context.Canceled", err)
	}

	outcome := make(chan GitActionOutcome, 1)
	go func() {
		got, err := app.GetGitActionOutcome(ctx, ref, action, "attempt-1")
		if err != nil {
			t.Error(err)
		}
		outcome <- got
	}()
	repeat := make(chan gitAnswer, 1)
	go func() {
		result, err := write("attempt-1")
		repeat <- gitAnswer{result, err}
	}()
	select {
	case got := <-outcome:
		t.Fatalf("the outcome answered before the %s ended: %+v", action, got)
	case <-time.After(100 * time.Millisecond):
	}

	gate.release()
	done := <-first
	if done.err != nil {
		t.Fatalf("%s: %v", action, done.err)
	}
	if got := <-outcome; got.State != "succeeded" || got.Result == nil || *got.Result != done.result {
		t.Fatalf("outcome = %+v, want %+v", got, done.result)
	}
	if again := <-repeat; again.err != nil || again.result != done.result {
		t.Fatalf("repeated attempt = %+v, %v; want the first one's answer", again.result, again.err)
	}
	if n := gate.runs(); n != 1 {
		t.Fatalf("the %s ran %d times; a repeated attempt wrote again", action, n)
	}

	// Another action, or another workspace, does not see the attempt.
	for _, other := range []struct {
		action string
		ref    WorkspaceRef
	}{
		{otherGitAction(action), ref},
		{action, WorkspaceRef{ProjectID: ref.ProjectID, WorkspacePath: ref.WorkspacePath + "-other"}},
	} {
		if got, err := app.GetGitActionOutcome(ctx, other.ref, other.action, "attempt-1"); err != nil || got.State != "unknown" {
			t.Fatalf("outcome of %s in %s = %+v, %v", other.action, other.ref.WorkspacePath, got, err)
		}
	}
	return done.result
}

func otherGitAction(action string) string {
	if action == gitActionPush {
		return gitActionPull
	}
	return gitActionPush
}

// A commit whose answer the client lost is answered from its outcome, and a
// repeat of the attempt commits nothing more.
func TestACommitAttemptIsAnsweredFromItsOutcome(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	hook := holdHook(t, workspace, "pre-commit")
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nheld\n")
	before := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD"))

	committed := heldAttempt(t, app, ref, gitActionCommit, hook, func(id string) (gitops.GitActionResult, error) {
		return app.GitCommit(ref, "held commit", "", CommitSelection{All: true}, id)
	})
	head := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD"))
	if committed.Commit != head || head == before {
		t.Fatalf("commit %q, HEAD %q (was %q)", committed.Commit, head, before)
	}
	if parent := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD~1")); parent != before {
		t.Fatalf("HEAD~1 = %s, want %s: more than one commit landed", parent, before)
	}
	other := WorkspaceRef{ProjectID: ref.ProjectID, WorkspacePath: ref.WorkspacePath + "-other"}
	if _, err := app.GitCommit(other, "elsewhere", "", CommitSelection{All: true}, "attempt-1"); err == nil {
		t.Fatal("an attempt id reused in another workspace committed")
	}

	// A commit that fails says so, with its own message.
	if _, err := app.GitCommit(ref, "nothing", "", CommitSelection{All: true}, "attempt-2"); err == nil {
		t.Fatal("a commit with nothing to commit succeeded")
	}
	if got, err := app.GetGitActionOutcome(context.Background(), ref, gitActionCommit, "attempt-2"); err != nil || got.State != "failed" || got.Message == "" {
		t.Fatalf("outcome of the failed commit = %+v, %v", got, err)
	}
}

// A push whose answer the client lost is answered from its outcome, and a
// repeat of the attempt pushes nothing more.
func TestAPushAttemptIsAnsweredFromItsOutcome(t *testing.T) {
	t.Parallel()
	app, ref, workspace, remote := gitActionTestSetup(t)
	testutil.RunGit(t, workspace, "remote", "add", "origin", remote)
	writeWorkspaceFile(t, workspace, "README.txt", "hello\npushed\n")
	if _, err := app.GitCommit(ref, "to push", "", CommitSelection{All: true}, ""); err != nil {
		t.Fatal(err)
	}
	hook := holdHook(t, workspace, "pre-push")

	pushed := heldAttempt(t, app, ref, gitActionPush, hook, func(id string) (gitops.GitActionResult, error) {
		return app.GitPush(ref, id)
	})
	if pushed.Action != gitActionPush {
		t.Fatalf("push result = %+v", pushed)
	}
	head := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(gitText(t, remote, "rev-parse", "refs/heads/main")); got != head {
		t.Fatalf("remote main = %s, want %s", got, head)
	}
}

// A pull request whose answer the client lost is answered from its outcome,
// and a repeat of the attempt runs the forge CLI's create no more.
func TestACreatePRAttemptIsAnsweredFromItsOutcome(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	testutil.RunGit(t, workspace, "remote", "add", "origin", "https://github.com/test/test.git")
	gh := newHeldGate(t)
	ghPath := filepath.Join(t.TempDir(), "gh")
	mockexec.Write(t, ghPath, fmt.Sprintf("#!/bin/sh\n%secho https://example.com/pr/7\n", gh.script()))
	app.forgeCLIs.fake = ghPath

	created := heldAttempt(t, app, ref, gitActionPR, gh, func(id string) (gitops.GitActionResult, error) {
		return app.GitCreatePR(t.Context(), ref, "Held PR", "", false, id)
	})
	if created.PRURL != "https://example.com/pr/7" {
		t.Fatalf("created = %+v", created)
	}
}

// An unknown action is refused rather than answered "unknown".
func TestGitActionOutcomeRefusesAnUnknownAction(t *testing.T) {
	t.Parallel()
	app, ref, _, _ := gitActionTestSetup(t)
	if _, err := app.GetGitActionOutcome(context.Background(), ref, "rebase", "attempt-1"); err == nil {
		t.Fatal("an unknown action was answered")
	}
}
