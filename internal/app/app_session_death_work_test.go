package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// passThreadLock takes and releases the thread action lock, so death work
// that escaped the group and already holds it has finished.
func passThreadLock(app *App, threadID string) {
	unlock := app.threadLocks().Lock(threadID)
	unlock()
}

// deathEvents feeds a session's abnormal end, "error" then "disconnected",
// through its handler and waits for the worker to handle both.
func deathEvents(t *testing.T, app *App, threadID, token string) {
	t.Helper()
	handler := startedSessionEventHandler(app, threadID, token, string(provider.Claude))
	for _, content := range []string{"error", "disconnected"} {
		handler(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: threadID, Content: content, Timestamp: time.Now()})
	}
	waitProviderEvents(t, app, threadID)
}

// A reconnect that a death started before shutdown, still starting its
// session when shutdown begins, finishes before the session close, which
// sees and closes the session it registered.
func TestShutdownJoinsAReconnectBeforeClosingSessions(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-death-work-joined")
	thread.Provider = string(provider.Claude)
	thread.SessionRef = "claude-resume-joined"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "token-dead"})
	entered, release := make(chan struct{}), make(chan struct{})
	var registered atomic.Bool
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(threadID string) error {
		close(entered)
		<-release
		app.sessionManager().put(threadID, session{Provider: string(provider.Claude), Token: "token-reconnected"})
		registered.Store(true)
		return nil
	}
	deathEvents(t, app, thread.ID, "token-dead")
	<-entered

	registeredAtClose := make(chan bool, 1)
	app.shutdownStepFn = func(step string, _ error) {
		switch step {
		case "stop idle session reaper":
			// The reconnect is let go just before shutdown stops the death
			// work, and takes longer than the steps up to the session close.
			time.AfterFunc(100*time.Millisecond, func() { close(release) })
		case "close provider sessions":
			registeredAtClose <- registered.Load()
		}
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !<-registeredAtClose {
		t.Fatal("the session close ran before the reconnect in flight registered its session")
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("the reconnect's session outlived shutdown's session close")
	}
}

// Once shutdown has stopped the death work, a death's restore, reconnect
// and startup-failure teardown run nothing: the queue stays where the next
// boot restores it from, and the session stays for shutdown to close.
func TestSessionDeathWorkOfferedAfterTheStopDoesNotRun(t *testing.T) {
	t.Parallel()
	t.Run("restore and reconnect", func(t *testing.T) {
		t.Parallel()
		app, _ := newAppForFlushQueueRPC(t)
		var starts atomic.Int32
		app.startSessionFn = func(string) error { starts.Add(1); return nil }
		thread := testThread("thread-death-after-stop")
		thread.Provider = string(provider.Claude)
		thread.WorkspacePath = t.TempDir()
		thread.SessionRef = "sess-dead"
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
		app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
			ID: "queue:after-stop", Message: "queued before the death", Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
		})

		app.sessionDeathWork.Close()
		deathEvents(t, app, thread.ID, "session-token")
		app.sessionDeathWork.Wait()
		passThreadLock(app, thread.ID)

		if draft, _, err := app.store.GetThreadDraft(thread.ID); err != nil || draft.Content != "" {
			t.Fatalf("draft = %q, %v; want nothing restored after the stop", draft.Content, err)
		}
		if !app.triage.HasQueuedFlushItems(thread.ID) {
			t.Fatal("the queue was drained after the stop")
		}
		if got := starts.Load(); got != 0 {
			t.Fatalf("%d reconnects started after the stop", got)
		}
	})
	t.Run("startup-failure teardown", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithStore(t)
		thread := testThread("thread-startup-failure-after-stop")
		thread.Provider = string(provider.Claude)
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "token-doa"})

		app.sessionDeathWork.Close()
		handler := startedSessionEventHandler(app, thread.ID, "token-doa", string(provider.Claude))
		handler(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: thread.ID, Timestamp: time.Now(),
			Failure: &provider.FailureMeta{Reason: provider.FailureReasonStartup},
		})
		waitProviderEvents(t, app, thread.ID)
		app.sessionDeathWork.Wait()
		passThreadLock(app, thread.ID)

		if _, live := app.sessionManager().get(thread.ID); !live {
			t.Fatal("the startup-failure teardown ran after the stop")
		}
	})
}

// A reconnect that shutdown refused, or that failed on the ended app
// context, is not a failure the person can act on, so it writes no error
// row. Any other failure, a cancellation while the app runs included,
// does.
func TestAutoReconnectRefusedByShutdownWritesNoErrorRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		appEnded   bool
		wantErrRow bool
	}{
		{name: "shutting down", err: fmt.Errorf("start session: %w", ErrShuttingDown)},
		{name: "app context ended", err: fmt.Errorf("start session: %w", context.Canceled), appEnded: true},
		{name: "cancelled while the app runs", err: fmt.Errorf("start session: %w", context.Canceled), wantErrRow: true},
		{name: "other failure", err: errors.New("chdir /gone: no such file or directory"), wantErrRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			thread := testThread("thread-reconnect-refused")
			thread.Provider = string(provider.Claude)
			thread.SessionRef = "claude-resume-refused"
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			errorItems := collectErrorItemUpserts(t, app, 4)
			appCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			app.appCtx = appCtx
			app.stopSessionFn = func(string) error { return nil }
			app.startSessionFn = func(string) error {
				if tc.appEnded {
					cancel()
				}
				return tc.err
			}

			app.attemptAutoReconnect(thread.ID)

			select {
			case item := <-errorItems:
				if !tc.wantErrRow {
					t.Fatalf("error row %q written for a reconnect shutdown refused", item.Summary)
				}
			default:
				if tc.wantErrRow {
					t.Fatal("no error row for a reconnect failure the person can act on")
				}
			}
		})
	}
}

// The close hook stands in only for closing a session the registry held:
// a stop of a thread with no session runs the real close.
func TestCloseHookAnswersOnlyForARegisteredSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-close-hook")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	app.closeProviderSessionFn = func(string) error { calls.Add(1); return nil }

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession with no session: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the close hook ran %d times for a thread with no session", got)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "tok"})
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the close hook ran %d times for a registered session, want 1", got)
	}
}
