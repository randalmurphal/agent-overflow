package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/shellrun"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// The gates of RunThreadShellCommand, called the way a paired phone or a
// connected browser calls it: on a connection carrying a session. The
// terminal:operate floor is the dispatcher's; every check below is the
// method's own. The autonomy check is in TestDrivingAnAutonomousThreadNeedsAutonomy
// with every other method that drives a thread.

// shellSessionApp is newShellCommandApp with the identity core, and a
// session holding every grant.
func shellSessionApp(t *testing.T) (*App, *emitRecorder, string) {
	t.Helper()
	app, rec := newShellCommandApp(t)
	if err := app.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	full := pairSessionWithScopes(t, app, "thumb-shell-full", identity.Scopes)
	return app, rec, full.ID
}

// refusedShellCommand starts a command that would leave a file behind and
// returns the error, after checking that nothing started.
func refusedShellCommand(t *testing.T, app *App, ctx context.Context, threadID string) error {
	t.Helper()
	ran := filepath.Join(t.TempDir(), "ran")
	_, err := app.RunThreadShellCommand(ctx, threadID, ShellCommandRequest{
		RunID: uuid.NewString(), Command: "touch " + ran,
	})
	if err == nil {
		t.Fatal("the command was admitted")
	}
	if runs, listErr := app.GetThreadShellCommands(threadID); listErr != nil || len(runs) != 0 {
		t.Fatalf("a refused command left a run: %+v %v", runs, listErr)
	}
	if _, statErr := os.Stat(ran); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused command ran: %v", statErr)
	}
	return err
}

// The output is sent to the thread, so a session that may run commands but
// not operate threads is refused, and the same call from a session holding
// both runs. The thread needs no autonomy, so threads:operate is the only
// grant between the two.
func TestRunThreadShellCommandNeedsThreadsOperate(t *testing.T) {
	app, rec, full := shellSessionApp(t)
	terminalOnly := pairSessionWithScopes(t, app, "thumb-shell-terminal", []identity.Scope{
		identity.ScopeThreadsRead, identity.ScopeTerminalOperate,
	})
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-operate")
	if err := app.store.UpdateRuntimeMode(thread.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatal(err)
	}

	wantScopeRefusal(t, refusedShellCommand(t, app, callFrom(terminalOnly.ID, false), thread.ID),
		transport.ScopeThreadsOperate)

	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(callFrom(full, false), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo admitted",
	}); err != nil {
		t.Fatalf("a session holding both grants: %v", err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	waitForCapturedUserMessages(t, capturePath, 1)
}

// A thread the app may not run in (a fork still being prepared, a thread
// moved to another computer) is refused before the command starts.
func TestRunThreadShellCommandChecksExecutionAccess(t *testing.T) {
	app, _, full := shellSessionApp(t)
	thread := testThread("thread-shell-preparing")
	thread.Provider = "claude"
	thread.WorkspacePath = t.TempDir()
	thread.ForkPreparing = true
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := refusedShellCommand(t, app, callFrom(full, false), thread.ID); !errors.Is(err, store.ErrForkPreparing) {
		t.Fatalf("err = %v, want %v", err, store.ErrForkPreparing)
	}
}

// A terminal thread has no agent to read the output.
func TestRunThreadShellCommandRefusesATerminalThread(t *testing.T) {
	app, _, full := shellSessionApp(t)
	thread := testThread("thread-shell-terminal")
	thread.Provider = "claude"
	thread.WorkspacePath = t.TempDir()
	thread.Mode = threadmode.ModeTerminal
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := refusedShellCommand(t, app, callFrom(full, false), thread.ID); !strings.Contains(err.Error(), "terminal thread") {
		t.Fatalf("err = %v, want the terminal-thread refusal", err)
	}
}

// The command gets the person's environment without the app's own AO_
// variables, which carry the app's credentials and test switches.
func TestRunThreadShellCommandDropsTheAppsOwnEnvironment(t *testing.T) {
	app, rec, full := shellSessionApp(t)
	t.Setenv("AO_SHELL_TEST_SECRET", "app-only")
	t.Setenv("SHELL_TEST_KEPT", "person")
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-env")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(callFrom(full, false), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "env",
	}); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	text := waitForCapturedUserMessages(t, capturePath, 1)[0]
	if !strings.Contains(text, "SHELL_TEST_KEPT=person") {
		t.Fatalf("the person's environment is missing from the command's:\n%s", text)
	}
	if strings.Contains(text, "\nAO_") || strings.Contains(text, ">AO_") {
		t.Fatalf("the command saw the app's AO_ variables:\n%s", text)
	}
}
