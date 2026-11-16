package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/providerupdate/updatefake"
	"agent-overflow/internal/shellrun"
	"agent-overflow/internal/terminal"

	"github.com/google/uuid"
)

// startTestShellCommand starts a composer shell command for threadID in dir
// that runs until it is stopped, and returns its run id.
func startTestShellCommand(t *testing.T, app *App, threadID, dir string) string {
	t.Helper()
	if app.shellCommands == nil {
		if err := app.initShellCommands(t.TempDir()); err != nil {
			t.Fatalf("initShellCommands: %v", err)
		}
		t.Cleanup(app.stopShellCommands)
	}
	runID := uuid.NewString()
	if _, err := app.shellCommands.Start(shellrun.Request{
		ThreadID: threadID, RunID: runID, Command: "sleep 60", Dir: dir, Shell: "/bin/sh",
	}); err != nil {
		t.Fatalf("start shell command: %v", err)
	}
	return runID
}

// stopTestShellCommand stops a command and waits until it is no longer
// registered.
func stopTestShellCommand(t *testing.T, app *App, threadID string) {
	t.Helper()
	app.shellCommands.CancelThread(threadID)
	deadline := time.Now().Add(10 * time.Second)
	for app.shellCommands.ActiveRunID(threadID) != "" {
		if time.Now().After(deadline) {
			t.Fatal("the stopped command is still registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The unattended merged-PR cleanup waits while a composer shell command runs
// in the worktree, or a terminal is open there: removing the worktree would
// stop both.
func TestMergedPRWorktreeBusyWaitsForAShellCommandAndATerminal(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	repo := initMainGitRepo(t)
	thread, path := mergeCleanupThread(t, app, repo, "feature/shell")

	startTestShellCommand(t, app, thread.ID, path)
	if err := app.mergedPRWorktreeBusy(path, []string{thread.ID}); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("a running shell command: got %v, want a shell-command refusal", err)
	}
	stopTestShellCommand(t, app, thread.ID)

	// A command a thread started before it moved away still runs in the
	// directory, and the directory is what goes.
	startTestShellCommand(t, app, "thread-moved-away", path)
	if err := app.mergedPRWorktreeBusy(path, []string{thread.ID}); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("another thread's command in the worktree: got %v, want a shell-command refusal", err)
	}
	stopTestShellCommand(t, app, "thread-moved-away")

	app.terminals = terminal.NewManager(nil, nil)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })
	summary, err := app.terminals.Open("draft:placeholder", terminal.SessionOptions{Reaper: procutiltest.Reaper(), Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: path})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	if err := app.mergedPRWorktreeBusy(path, []string{thread.ID}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("an open terminal: got %v, want a terminal refusal", err)
	}
	if err := app.terminals.Close(summary.TerminalID); err != nil {
		t.Fatalf("close terminal: %v", err)
	}
	if err := app.mergedPRWorktreeBusy(path, []string{thread.ID}); err != nil {
		t.Fatalf("after the command stopped and the terminal closed: %v", err)
	}
}

// The whole cleanup with a command running in the worktree: it waits, keeps
// the command running, and removes the worktree once the command is gone.
func TestMergedPRCleanupWaitsForAShellCommand(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/shell-wait")
	startTestShellCommand(t, m.app, m.thread.ID, m.path)

	m.pass()
	m.expectWorktree(t, true)
	m.expectOutcome(t, "")
	if m.app.shellCommands.ActiveRunID(m.thread.ID) == "" {
		t.Fatal("the cleanup stopped the shell command")
	}

	stopTestShellCommand(t, m.app, m.thread.ID)
	m.due()
	m.pass()
	m.expectWorktree(t, false)
	m.expectOutcome(t, "removed")
}

// A worktree removal the user asks for stops the shell commands running in
// it, and the removal goes ahead.
func TestRemovingAWorktreeStopsItsShellCommands(t *testing.T) {
	t.Parallel()
	f := newRemovalFixture(t, "feature-shell-removal")
	startTestShellCommand(t, f.app, f.occupant, f.worktree)
	startTestShellCommand(t, f.app, f.caller, f.repo)

	f.remove(t)

	if f.app.shellCommands.ActiveRunID(f.occupant) != "" {
		t.Fatal("the command running in the removed worktree survived the removal")
	}
	if f.app.shellCommands.ActiveRunID(f.caller) == "" {
		t.Fatal("the removal stopped a command running outside the worktree")
	}
}

// A thread does not move to another checkout while its composer shell
// command runs: the command's output would reach the agent in a directory
// the command never ran in.
func TestAThreadDoesNotChangeWorkspaceWhileItsShellCommandRuns(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	repo := initMainGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	thread := testThread("thread-shell-switch")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	other, err := app.GitCreateWorktree(thread.ID, "feature/shell-target")
	if err != nil {
		t.Fatalf("GitCreateWorktree: %v", err)
	}
	startTestShellCommand(t, app, thread.ID, repo)

	if _, err := app.PrepareThreadWorktree(thread.ID, "", "feature/shell-new", false); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("PrepareThreadWorktree while a command runs: %v", err)
	}
	if _, err := app.AttachThreadWorktree(thread.ID, "main"); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("AttachThreadWorktree while a command runs: %v", err)
	}
	if _, err := app.switchThreadWorkspace(thread.ID, other); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("switch workspace while a command runs: %v", err)
	}

	stopTestShellCommand(t, app, thread.ID)
	if _, err := app.switchThreadWorkspace(thread.ID, other); err != nil {
		t.Fatalf("switch workspace after the command stopped: %v", err)
	}
}

// A command started while a workspace switch or a removal holds the thread
// waits for it, so it runs where the thread is after the move: their
// running-work check cannot miss it.
func TestAShellCommandStartWaitsForTheThreadActionLock(t *testing.T) {
	app, _ := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-action-lock")
	unlock := app.threadLocks().Lock(thread.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := app.RunThreadShellCommand(ctx, thread.ID, ShellCommandRequest{RunID: uuid.NewString(), Command: "sleep 60"})
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunThreadShellCommand under a held action lock = %v, want it to wait", err)
	}
	if app.shellCommands.ActiveRunID(thread.ID) != "" {
		t.Fatal("a command started while the thread's action lock was held")
	}
}

// A conversation does not leave this computer while its composer shell
// command runs here.
func TestATransferWaitsForTheShellCommand(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, string(provider.Claude))
	if err := a.checkTransferIdle(thread); err != nil {
		t.Fatalf("an idle conversation: %v", err)
	}
	startTestShellCommand(t, a, thread.ID, thread.WorkspacePath)
	if err := a.checkTransferIdle(thread); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("a running shell command: got %v, want a shell-command refusal", err)
	}
}

// The restart to update waits for a provider CLI update: shutting down kills
// the package manager mid-install. And an update cannot start once that wait
// found the computer idle.
func TestAnUpdateWaitsForAProviderCLIUpdate(t *testing.T) {
	app := newTestAppWithStore(t)
	release := armBlockingProviderUpdate(t, app)
	if err := app.RunProviderUpdate(callSteppedUp("owner"), string(provider.Claude)); err != nil {
		t.Fatalf("RunProviderUpdate: %v", err)
	}
	if reason, err := app.updateWorkReason(); err != nil || !strings.Contains(reason, "provider CLI update") {
		t.Fatalf("reason while the update runs: %q %v", reason, err)
	}
	release()
	deadline := time.Now().Add(15 * time.Second)
	for {
		reason, err := app.updateWorkReason()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reason, "provider CLI update") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reason after the update ended: %q", reason)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Admission closed by an idle check: a new update waits for it.
	if reason, err := app.workAdmission.quiesce(func() (string, error) { return "", nil }); err != nil || reason != "" {
		t.Fatalf("quiesce: %q %v", reason, err)
	}
	ctx, cancel := context.WithTimeout(callSteppedUp("owner"), 100*time.Millisecond)
	defer cancel()
	if err := app.RunProviderUpdate(ctx, string(provider.Claude)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunProviderUpdate with admission closed = %v, want it to wait", err)
	}
	app.workAdmission.reopen()
}

// armBlockingProviderUpdate offers a Claude update whose command runs until
// the returned release is called. Serial: it resets the Claude probe cache
// and stubs version detection.
func armBlockingProviderUpdate(t *testing.T, app *App) (release func()) {
	t.Helper()
	resetClaudeProbeCacheForTest(t)
	binary := filepath.Join(t.TempDir(), "claude")
	writeClaudeProbeMockAt(t, binary, "99.0.0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Claude) {
			return "0.149.0"
		}
		return updatefake.Baseline
	})
	if err := app.providerUpdateFeed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(done) }) }
	t.Cleanup(release)
	app.providerUpdateRunner = func(ctx context.Context, command providerupdate.Command) (providerupdate.Output, error) {
		if command.Args[0] == "doctor" {
			return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
		return providerupdate.Output{ExitCode: 1}, nil
	}
	statuses, err := app.CheckProviderUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if claude := providerUpdateStatus(t, statuses, string(provider.Claude)); !claude.Available {
		t.Fatalf("claude = %+v, want an update offered", claude)
	}
	return release
}
