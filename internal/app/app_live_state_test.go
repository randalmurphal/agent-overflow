package app

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func seedLiveStateTodoThread(t *testing.T, app *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

// The auto-hide age filter lives on the read side now, and it must agree with
// the frontend's LIVE_TODO_AUTOHIDE_MS: a finished list the pane already hid
// must not come back on refresh, while a list with work left in it is exactly
// what a returning user came for.
func TestGetThreadLiveStateAppliesTodoAutoHide(t *testing.T) {
	cases := []struct {
		name     string
		steps    string
		age      time.Duration
		wantTodo bool
	}{
		{
			name:     "all completed and older than the auto-hide window",
			steps:    `[{"step":"done","status":"completed"}]`,
			age:      time.Duration(liveTodoAutoHideMillis)*time.Millisecond + time.Second,
			wantTodo: false,
		},
		{
			name:     "all completed but still fresh",
			steps:    `[{"step":"done","status":"completed"}]`,
			age:      0,
			wantTodo: true,
		},
		{
			name:     "unfinished work never ages out",
			steps:    `[{"step":"done","status":"completed"},{"step":"next","status":"pending"}]`,
			age:      time.Duration(liveTodoAutoHideMillis)*time.Millisecond + time.Hour,
			wantTodo: true,
		},
		{
			// A backward clock step between the report and the refresh makes
			// the age negative. The frontend's hydrate filter fails closed on
			// that (`age >= 0`), and the backend must agree — serving the
			// list forever here is the lockstep the two comments promise.
			name:     "all completed with a future timestamp fails closed",
			steps:    `[{"step":"done","status":"completed"}]`,
			age:      -time.Hour,
			wantTodo: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := newAppForFlushQueueRPC(t)
			thread := seedLiveStateTodoThread(t, app, "live-todo-ttl")

			if err := app.triage.Handle(provider.ProviderEvent{
				Kind:      provider.EventTodoUpdate,
				ThreadID:  thread.ID,
				Meta:      json.RawMessage(`{"plan":` + tc.steps + `}`),
				Timestamp: time.Now().Add(-tc.age),
			}); err != nil {
				t.Fatalf("todo update: %v", err)
			}

			state, err := app.GetThreadLiveState(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadLiveState: %v", err)
			}
			if tc.wantTodo {
				if state.Todo == nil {
					t.Fatalf("Todo = nil, want the stored list")
				}
				if state.Todo.ThreadID != thread.ID {
					t.Fatalf("Todo.ThreadID = %q, want %q", state.Todo.ThreadID, thread.ID)
				}
				return
			}
			if state.Todo != nil {
				t.Fatalf("Todo = %+v, want nil (aged out)", state.Todo)
			}
			// Aged out for the reader, still on the row: the filter is a
			// rendering decision, not a deletion.
			if _, found, err := app.store.ThreadLiveTodo(thread.ID); err != nil || !found {
				t.Fatalf("the aged-out list must still be stored; found=%v err=%v", found, err)
			}
		})
	}
}

// The durability contract end to end, and the whole point of migration v65:
// a list reported into one process is still there for the next one. The
// session is torn down and triage is replaced wholesale — a fresh router over
// the same store is what a restart leaves behind.
func TestGetThreadLiveStateReadsTodoWrittenByAPreviousSession(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := seedLiveStateTodoThread(t, app, "live-todo-restart")

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventTodoUpdate,
		ThreadID: thread.ID,
		Meta: json.RawMessage(
			`{"plan":[{"step":"written before the restart","status":"inProgress","id":"1","owner":"helper"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	app.triage.CleanupThread(thread.ID)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()

	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if state.Todo == nil || len(state.Todo.Steps) != 1 {
		t.Fatalf("Todo = %+v, want the list a previous session reported", state.Todo)
	}
	step := state.Todo.Steps[0]
	if step.Step != "written before the restart" || step.ID != "1" || step.Owner != "helper" {
		t.Fatalf("Todo.Steps[0] = %+v, want the stored step with its id and owner", step)
	}
	if state.Todo.ThreadID != thread.ID {
		t.Fatalf("Todo.ThreadID = %q, want %q", state.Todo.ThreadID, thread.ID)
	}
}

// A blob this build cannot read must cost the user the todo list and nothing
// else: the active turn, the queue, and pending approvals are what a refresh
// is actually for.
func TestGetThreadLiveStateSurvivesAnUnreadableTodo(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()
	thread := seedLiveStateTodoThread(t, app, "live-todo-corrupt")

	// A blob whose shape drifted from this build's: json_valid passes the
	// CHECK, the strict decoder refuses it. Written through a second handle
	// because no store accessor can produce one — which is the point.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(
		`UPDATE threads SET live_todo = ? WHERE id = ?`,
		`{"steps":[{"step":"one","status":"pending"}],"drifted":true}`, thread.ID,
	); err != nil {
		t.Fatalf("seed drifted blob: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 3,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState must survive an unreadable todo blob: %v", err)
	}
	if state.Todo != nil {
		t.Fatalf("Todo = %+v, want nil for an unreadable blob", state.Todo)
	}
	if state.ActiveTurn == nil || state.ActiveTurn.TurnIndex != 3 {
		t.Fatalf("ActiveTurn = %+v, want the live turn to survive the todo failure", state.ActiveTurn)
	}
}
