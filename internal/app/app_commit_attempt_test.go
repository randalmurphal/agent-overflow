package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// holdPreCommit installs a pre-commit hook that counts its runs in a file
// and waits until release is called, so a test holds a commit open as a
// slow hook would.
func holdPreCommit(t *testing.T, workspace string) (runs func() int, release func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	dir := t.TempDir()
	count, gate := filepath.Join(dir, "runs"), filepath.Join(dir, "release")
	hooks := strings.TrimSpace(gitText(t, workspace, "rev-parse", "--git-path", "hooks"))
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(workspace, hooks)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho run >> '" + count + "'\nwhile [ ! -f '" + gate + "' ]; do sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(release)
	runs = func() int {
		data, err := os.ReadFile(count)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "run")
	}
	return runs, release
}

// A commit whose answer the client lost, to a disconnect or its own wait,
// is answered from its outcome: GetCommitOutcome waits for the running
// commit and reports it, and a repeat of the attempt commits nothing more.
func TestACommitAttemptIsAnsweredFromItsOutcome(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	ctx := context.Background()
	runs, release := holdPreCommit(t, workspace)
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nheld\n")
	before := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD"))

	if outcome, err := app.GetCommitOutcome(ctx, ref, "attempt-1"); err != nil || outcome.State != "unknown" {
		t.Fatalf("outcome before the commit arrived = %+v, %v", outcome, err)
	}

	type answer struct {
		result gitops.GitActionResult
		err    error
	}
	first := make(chan answer, 1)
	go func() {
		result, err := app.GitCommit(ref, "held commit", "", CommitSelection{All: true}, "attempt-1")
		first <- answer{result, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for runs() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pre-commit hook never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A caller that leaves stops only its own wait.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := app.GetCommitOutcome(gone, ref, "attempt-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome for a caller that left = %v, want context.Canceled", err)
	}

	outcome := make(chan CommitOutcome, 1)
	go func() {
		got, err := app.GetCommitOutcome(ctx, ref, "attempt-1")
		if err != nil {
			t.Error(err)
		}
		outcome <- got
	}()
	repeat := make(chan answer, 1)
	go func() {
		result, err := app.GitCommit(ref, "held commit", "", CommitSelection{All: true}, "attempt-1")
		repeat <- answer{result, err}
	}()
	select {
	case got := <-outcome:
		t.Fatalf("the outcome answered before the commit ended: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	committed := <-first
	if committed.err != nil {
		t.Fatalf("GitCommit: %v", committed.err)
	}
	head := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD"))
	if committed.result.Commit != head || head == before {
		t.Fatalf("commit %q, HEAD %q (was %q)", committed.result.Commit, head, before)
	}
	if got := <-outcome; got.State != "committed" || got.Result == nil || got.Result.Commit != head {
		t.Fatalf("outcome = %+v, want the commit %s", got, head)
	}
	if again := <-repeat; again.err != nil || again.result.Commit != head {
		t.Fatalf("repeated attempt = %+v, %v; want the first one's answer", again.result, again.err)
	}
	if n := runs(); n != 1 {
		t.Fatalf("the hook ran %d times; a repeated attempt committed again", n)
	}
	if parent := strings.TrimSpace(gitText(t, workspace, "rev-parse", "HEAD~1")); parent != before {
		t.Fatalf("HEAD~1 = %s, want %s: more than one commit landed", parent, before)
	}

	// Another workspace does not see the attempt.
	other := WorkspaceRef{ProjectID: ref.ProjectID, WorkspacePath: ref.WorkspacePath + "-other"}
	if got, err := app.GetCommitOutcome(ctx, other, "attempt-1"); err != nil || got.State != "unknown" {
		t.Fatalf("outcome in another workspace = %+v, %v", got, err)
	}
	if _, err := app.GitCommit(other, "elsewhere", "", CommitSelection{All: true}, "attempt-1"); err == nil {
		t.Fatal("an attempt id reused in another workspace committed")
	}

	// A commit that fails says so, with its own message.
	if _, err := app.GitCommit(ref, "nothing", "", CommitSelection{All: true}, "attempt-2"); err == nil {
		t.Fatal("a commit with nothing to commit succeeded")
	}
	if got, err := app.GetCommitOutcome(ctx, ref, "attempt-2"); err != nil || got.State != "failed" || got.Message == "" {
		t.Fatalf("outcome of the failed commit = %+v, %v", got, err)
	}
}
