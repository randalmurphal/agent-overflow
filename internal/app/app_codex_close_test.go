package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/triage"
)

// codexCloseErrorRows returns the summaries of threadID's error rows.
func codexCloseErrorRows(t *testing.T, app *App, threadID string) []string {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows []string
	for _, item := range items {
		if item.Kind == triage.ItemKindError {
			rows = append(rows, item.Summary)
		}
	}
	return rows
}

// When Codex does not confirm that it closed an agent's thread at End
// Session, the thread shows an error that names the agent and the commands
// it ran, which may still be running, and nothing of a thread Codex did
// close. The session's end still drops triage's command state, and the
// error still reaches the caller.
func TestEndSessionNamesTheCommandsCodexMayStillRun(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-end-session-unclosed")
	seedCodexCommandRunning(t, app, threadID, "root-cmd", "1001", "", codexScope(rootCodexThread), "pnpm run dev")
	seedCodexCommandRunning(t, app, threadID, "agent-cmd", "1002", "spawn-1", codexScope(childCodexThread), "pytest -x")
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "tok"})
	app.closeProviderSessionFn = func(string) error {
		return fmt.Errorf("close codex session for thread %s: %w", threadID, errors.Join(&codex.ThreadNotClosedError{
			ThreadID: childCodexThread, Agent: "Reviewer", Err: errors.New("codex: timed out waiting for thread/closed on " + childCodexThread),
		}))
	}

	if err := app.StopSession(threadID); err == nil {
		t.Fatal("StopSession = nil, want the close's error")
	}
	rows := codexCloseErrorRows(t, app, threadID)
	if len(rows) != 1 {
		t.Fatalf("error rows = %q, want one", rows)
	}
	row := rows[0]
	for _, want := range []string{"agent Reviewer", `pytest -x"`, "may still be running", "timed out waiting for thread/closed"} {
		if !strings.Contains(row, want) {
			t.Fatalf("error row = %q, want it to contain %q", row, want)
		}
	}
	if strings.Contains(row, "pnpm run dev") || strings.Contains(row, "this thread") {
		t.Fatalf("error row = %q names the thread Codex closed", row)
	}
	if live := app.triage.LiveCodexCommands(threadID); len(live) != 0 {
		t.Fatalf("live commands after the session's end = %+v, want none", live)
	}
}

// A close Codex confirmed leaves no such row.
func TestEndSessionThatCodexConfirmsLeavesNoCloseError(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-end-session-closed")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "tok"})
	app.closeProviderSessionFn = func(string) error { return nil }

	if err := app.StopSession(threadID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if rows := codexCloseErrorRows(t, app, threadID); len(rows) != 0 {
		t.Fatalf("error rows = %q, want none", rows)
	}
}

// Every unconfirmed thread is named: the session's own as this thread, an
// agent Codex gave no name as an agent, and one that ran nothing says so.
func TestCodexUnclosedThreadsErrorNamesEachThread(t *testing.T) {
	t.Parallel()
	err := errors.Join(
		&codex.ThreadNotClosedError{ThreadID: "agent-thread", Err: errors.New("unsubscribe failed")},
		&codex.ThreadNotClosedError{ThreadID: "root", Root: true, Err: errors.New("timed out")},
	)
	text, unclosed := codexUnclosedThreadsError(fmt.Errorf("close: %w", err), []triage.CodexCommand{
		{ScopeID: "root", Summary: "sleep 60"},
		{ScopeID: "root", Summary: "npm run dev"},
		{ScopeID: "closed-agent", Summary: "make test"},
	})
	if !unclosed {
		t.Fatal("codexUnclosedThreadsError reported every thread closed")
	}
	for _, want := range []string{"- an agent: no command was running", `- this thread: "npm run dev", "sleep 60"`, "Codex: unsubscribe failed; timed out"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error = %q, want it to contain %q", text, want)
		}
	}
	if strings.Contains(text, "make test") {
		t.Fatalf("error = %q names a command of a thread Codex closed", text)
	}
	if _, unclosed := codexUnclosedThreadsError(errors.New("close failed"), nil); unclosed {
		t.Fatal("an error with no unconfirmed thread was reported as one")
	}
}
