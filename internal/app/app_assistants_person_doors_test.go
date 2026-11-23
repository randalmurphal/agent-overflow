package app

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// A person's doors on a paused assistant's thread: each one that would
// drive it, rewrite its conversation, fork, move or copy it, run a command
// in it or bind a workflow run to it refuses with the off message before it
// changes anything (admitPersonOperation).

// pauseHere turns this computer's pause on as turning assistants off does
// first, without the convergence that also stops their sessions: the doors
// are refused by the pause alone.
func pauseHere(t *testing.T, a *App) {
	t.Helper()
	a.store.PauseAssistants(true)
}

// threadSnapshot is what a refused door must leave as it was: the cached
// rows, the native session the thread resumes, its draft, any staged
// replacement, the threads there are and whether its session runs.
type threadSnapshot struct {
	items      []string
	sessionRef string
	draft      store.ThreadDraft
	recoveries int
	threads    int
	live       bool
}

func snapshotThread(t *testing.T, a *App, threadID string) threadSnapshot {
	t.Helper()
	items, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	snap := threadSnapshot{}
	for _, item := range items {
		snap.items = append(snap.items, item.ID+"|"+item.Summary)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	snap.sessionRef = thread.SessionRef
	if draft, found, err := a.store.GetThreadDraft(threadID); err != nil {
		t.Fatal(err)
	} else if found {
		snap.draft = draft
	}
	recoveries, err := a.store.ListThreadDraftRecoveries()
	if err != nil {
		t.Fatal(err)
	}
	snap.recoveries = len(recoveries)
	threads, err := a.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	snap.threads = len(threads)
	_, snap.live = a.sessionManager().get(threadID)
	return snap
}

func requireUnchanged(t *testing.T, a *App, threadID, door string, before threadSnapshot) {
	t.Helper()
	after := snapshotThread(t, a, threadID)
	if !slices.Equal(after.items, before.items) || after.sessionRef != before.sessionRef || after.draft != before.draft ||
		after.recoveries != before.recoveries || after.threads != before.threads || after.live != before.live {
		t.Fatalf("%s changed the paused thread:\n before %+v\n after  %+v", door, before, after)
	}
}

func requireAssistantsOff(t *testing.T, door string, err error) {
	t.Helper()
	if !errors.Is(err, errAssistantsOff) {
		t.Fatalf("%s on a paused assistant's thread = %v, want the off message", door, err)
	}
}

// Edit and resend, send, steer and the three forks of a paused assistant's
// Claude thread refuse, leaving its rows, its native session and its draft.
func TestAPausedAssistantsThreadRefusesAPersonsConversationDoors(t *testing.T) {
	t.Parallel()
	a, _ := newResendTestApp(t)
	thread, workspace := seedResendThread(t, a, "paused-doors")
	testAssistant(t, a, thread.ID)
	if _, err := a.store.UpsertThreadDraft(store.ThreadDraft{ThreadID: thread.ID, Content: "half typed", UpdatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	pauseHere(t, a)
	before := snapshotThread(t, a, thread.ID)
	ctx := context.Background()
	one := 0
	doors := []struct {
		name string
		call func() error
	}{
		{"edit and resend", func() error {
			_, err := a.RevertConversationAndResendMessage(ctx, thread.ID, "user:1", RevertAndResendOptions{Content: "edited", SendID: uuid.NewString()})
			return err
		}},
		{"send", func() error {
			_, err := a.SendMessageWithOptions(ctx, thread.ID, "hello", SendMessageOptions{SendID: uuid.NewString()})
			return err
		}},
		{"fork", func() error { _, err := a.ForkThread(ctx, thread.ID, &one); return err }},
		{"fork from a message", func() error { _, err := a.ForkThreadFromMessage(ctx, thread.ID, "user:1"); return err }},
		{"side chat", func() error { _, err := a.ForkSideChat(ctx, thread.ID); return err }},
	}
	for _, door := range doors {
		requireAssistantsOff(t, door.name, door.call())
		requireUnchanged(t, a, thread.ID, door.name, before)
	}
	assertClaudeSessionText(t, a, workspace, resendSourceSessionID, []string{"first", "second"}, nil)
}

// A shell command does not run in a paused assistant's thread.
func TestAPausedAssistantsThreadRunsNoShellCommand(t *testing.T) {
	a, _ := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, a, "paused-shell")
	testAssistant(t, a, thread.ID)
	pauseHere(t, a)
	before := snapshotThread(t, a, thread.ID)
	_, err := a.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{RunID: uuid.NewString(), Command: "echo hi"})
	requireAssistantsOff(t, "a shell command", err)
	requireUnchanged(t, a, thread.ID, "a shell command", before)
	if runs, err := a.GetThreadShellCommands(thread.ID); err != nil || len(runs) != 0 {
		t.Fatalf("runs after the refusal = %+v, %v", runs, err)
	}
}

// A workflow run is not bound to a paused assistant's thread.
func TestAPausedAssistantsThreadTakesNoWorkflowRun(t *testing.T) {
	t.Parallel()
	fixture := newCLIFixture(t)
	thread, err := fixture.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	result, err := fixture.app.workflowAgentStartRun(ctx, WorkflowAgentStartInput{WorkflowID: "tool-flow", Goal: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.WorkflowUnbindThread(result.ItemID); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	testAssistant(t, fixture.app, thread.ID)
	pauseHere(t, fixture.app)
	_, err = fixture.app.WorkflowBindThread(result.ItemID, thread.ID)
	requireAssistantsOff(t, "binding a workflow run", err)
	item, err := fixture.app.store.GetWorkItem(result.ItemID)
	if err != nil || item.OriginThreadID != "" {
		t.Fatalf("the run after the refused bind = %+v, %v; want it unbound", item, err)
	}
}

// A paused assistant's thread is neither copied nor moved to another
// computer.
func TestAPausedAssistantsThreadIsNotTransferred(t *testing.T) {
	remotetest.Require(t)
	source, destination := transferTestBackend(t), transferTestBackend(t)
	home := t.TempDir()
	for _, id := range []string{"past", "current"} {
		if err := source.store.CreateThread(store.Thread{ID: id, Title: "Ops", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired),
			WorkspacePath: home, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.store.CreateStandingAgent(store.StandingAgent{ID: "ops", ThreadID: "past", Home: home, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.HandOffAssistant("past", "current", 2); err != nil {
		t.Fatal(err)
	}
	destinationID, _ := destination.backendIdentity()
	pauseHere(t, source)
	for _, door := range []struct{ thread, kind string }{{"current", "copy"}, {"past", "copy"}, {"past", "move"}} {
		operation := uuid.NewString()
		_, err := source.BeginThreadTransfer(context.Background(), door.thread, operation, destinationID, door.kind, false)
		requireAssistantsOff(t, door.kind+" of "+door.thread, err)
		if _, err := source.store.GetThreadTransfer(operation); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the refused %s of %s left a transfer: %v", door.kind, door.thread, err)
		}
	}
}

// An operation admitted before the pause is not cut short by it: its
// session start goes ahead, and the pause stops that session once the
// operation lets go of the thread's action lock. A start nobody admitted is
// refused.
func TestAnOperationAdmittedBeforeThePauseFinishes(t *testing.T) {
	t.Parallel()
	a, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, a, "admitted-before")
	testAssistant(t, a, thread.ID)
	admitted, err := a.admitPersonOperation(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	pauseHere(t, a)
	if err := a.startSession(context.Background(), thread.ID); !errors.Is(err, errAssistantsOff) {
		t.Fatalf("a start nobody admitted = %v, want the off message", err)
	}
	if err := a.startSession(admitted, thread.ID); err != nil {
		t.Fatalf("the admitted operation's start = %v, want it to go ahead", err)
	}
}
