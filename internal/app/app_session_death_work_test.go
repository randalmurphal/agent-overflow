package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

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

// closeRecordingSession is a provider session that only records its Close,
// which onClose observes and closeErr answers.
type closeRecordingSession struct {
	provider.Session
	closes   atomic.Int32
	onClose  func()
	closeErr error
}

func (s *closeRecordingSession) Close() error {
	if s.closes.Add(1) == 1 && s.onClose != nil {
		s.onClose()
	}
	return s.closeErr
}

func (s *closeRecordingSession) PID() int                                { return 0 }
func (s *closeRecordingSession) Connection() provider.Connection         { return nil }
func (s *closeRecordingSession) OwnedProcesses() []provider.OwnedProcess { return nil }

// reconnectableThread stores a Claude thread with a resume cursor, which a
// reconnect needs.
func reconnectableThread(t *testing.T, app *App, id string) string {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	thread.SessionRef = "claude-resume-" + id
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return thread.ID
}

// waitClosed waits for ch to close. The bound only ends a test whose
// event never comes.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A reconnect's start can block on a provider write until shutdown's
// session close ends that provider. Shutdown joins the death work after
// that close, so it does not wait on it; the start that finishes then finds
// the registry sealed and closes the session it started.
func TestQuitDoesNotWaitOnAReconnectTheSessionCloseReleases(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := reconnectableThread(t, app, "thread-death-work-released")
	// Stands in for the pooled connection whose write the start waits on:
	// it answers only once Step 4 closes the sessions on it.
	released := make(chan struct{})
	app.sessionManager().put("thread-on-the-connection", session{
		Provider: string(provider.Claude), Token: "token-on-the-connection",
		Session: &closeRecordingSession{onClose: func() { close(released) }},
	})
	reconnected := &closeRecordingSession{}
	entered := make(chan struct{})
	registration := make(chan error, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(id string) error {
		close(entered)
		select {
		case <-released:
		case <-time.After(10 * time.Second):
			registration <- errors.New("shutdown joined the start before the session close released it")
			return errors.New("start never released")
		}
		err := app.registerStartedSession(id, session{Provider: string(provider.Claude), Token: "token-reconnected", Session: reconnected})
		registration <- err
		return err
	}
	deathEvents(t, app, threadID, "token-dead")
	waitClosed(t, entered, "the reconnect to start its session")

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-registration; !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("registration after the session close = %v, want it refused by shutdown", err)
	}
	if got := reconnected.closes.Load(); got != 1 {
		t.Fatalf("the refused start's session was closed %d times, want 1", got)
	}
	if _, live := app.sessionManager().get(threadID); live {
		t.Fatal("the reconnect's session outlived shutdown")
	}
}

// After the registry is sealed, a start that finished registers nothing,
// closes what it started and says why; a close failure comes back with the
// refusal and is not taken for one.
func TestARegistrationAfterTheSealClosesWhatItStarted(t *testing.T) {
	t.Parallel()
	for _, closeErr := range []error{nil, errors.New("injected close failure")} {
		t.Run(fmt.Sprintf("close failure %v", closeErr != nil), func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.sessionManager().sealAndSnapshot()
			started := &closeRecordingSession{closeErr: closeErr}

			err := app.registerStartedSession("thread-after-seal", session{Provider: string(provider.Claude), Token: "token-late", Session: started})

			if !errors.Is(err, ErrShuttingDown) {
				t.Fatalf("registration = %v, want it refused by shutdown", err)
			}
			if closeErr != nil && (!errors.Is(err, closeErr) || app.refusedByShutdown(err)) {
				t.Fatalf("registration = %v, want the close failure reported as a failure", err)
			}
			if got := started.closes.Load(); got != 1 {
				t.Fatalf("the session was closed %d times, want 1", got)
			}
			if _, live := app.sessionManager().get("thread-after-seal"); live {
				t.Fatal("the sealed registry registered the session")
			}
		})
	}
}

// A restore a death started before quit, still waiting on the thread lock
// when the join begins, runs to its end inside the join and before the
// store closes. Its reconnect is refused, because shutdown has begun.
func TestARestoreDuringQuitFinishesBeforeTheStoreCloses(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error { starts.Add(1); return nil }
	threadID := reconnectableThread(t, app, "thread-restore-during-quit")
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{
		ID: "queue:during-quit", Message: "queued before the death", Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
	})
	unlock := app.threadLocks().Lock(threadID)
	deathEvents(t, app, threadID, "token-dead")

	joinBegan := make(chan bool, 1)
	draftAtJoin := make(chan string, 1)
	app.shutdownStepFn = func(step string, _ error) {
		switch step {
		case "stop orphan reaper":
			// The next step is the join: the lock is let go once the join
			// refuses new work, so the restore runs inside it.
			go func() {
				deadline := time.Now().Add(10 * time.Second)
				for app.sessionDeathWork.Go(func() {}) {
					if time.Now().After(deadline) {
						joinBegan <- false
						unlock()
						return
					}
					time.Sleep(time.Millisecond)
				}
				joinBegan <- true
				unlock()
			}()
		case "stop session death work":
			draft, _, err := app.store.GetThreadDraft(threadID)
			if err != nil {
				draftAtJoin <- "read failed: " + err.Error()
				return
			}
			draftAtJoin <- draft.Content
		}
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !<-joinBegan {
		t.Fatal("shutdown never joined the death work")
	}
	if draft := <-draftAtJoin; !strings.Contains(draft, "queued before the death") {
		t.Fatalf("draft after the join = %q, want the queued message restored", draft)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("%d reconnects started during quit", got)
	}
}

// Every piece of work a session's end starts is a member of the death-work
// group, which is what Shutdown and the test fixtures join. The thread lock
// the work waits on keeps each member running while it is counted.
func TestEveryPieceOfSessionDeathWorkIsJoined(t *testing.T) {
	t.Parallel()
	t.Run("startup-failure teardown", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithTriage(t)
		threadID := reconnectableThread(t, app, "thread-startup-failure-joined")
		app.sessionManager().put(threadID, session{Provider: string(provider.Claude), Token: "token-doa"})
		unlock := app.threadLocks().Lock(threadID)
		handler := startedSessionEventHandler(app, threadID, "token-doa", string(provider.Claude))
		handler(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: threadID, Timestamp: time.Now(),
			Failure: &provider.FailureMeta{Reason: provider.FailureReasonStartup},
		})
		waitProviderEvents(t, app, threadID)
		members := app.sessionDeathWork.members()
		unlock()
		app.sessionDeathWork.Wait()
		if members != 1 {
			t.Fatalf("%d death-work members after a startup failure, want 1", members)
		}
		if _, live := app.sessionManager().get(threadID); live {
			t.Fatal("the startup-failure teardown left the session registered")
		}
	})
	t.Run("restore and reconnect", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithTriage(t)
		var starts atomic.Int32
		app.stopSessionFn = func(string) error { return nil }
		app.startSessionFn = func(string) error { starts.Add(1); return nil }
		threadID := reconnectableThread(t, app, "thread-death-joined")
		unlock := app.threadLocks().Lock(threadID)
		deathEvents(t, app, threadID, "token-dead")
		members := app.sessionDeathWork.members()
		unlock()
		app.sessionDeathWork.Wait()
		if members != 2 {
			t.Fatalf("%d death-work members after a death, want 2 (the restore, then the restore and reconnect)", members)
		}
		if got := starts.Load(); got != 1 {
			t.Fatalf("%d reconnects after the death, want 1", got)
		}
	})
}

// Once shutdown has joined the death work, a death's work is refused and
// runs nothing: the queue stays where the next boot restores it from, and
// nothing reconnects.
func TestSessionDeathWorkOfferedAfterTheJoinRunsNothing(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error { starts.Add(1); return nil }
	threadID := reconnectableThread(t, app, "thread-death-after-join")
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{
		ID: "queue:after-join", Message: "queued before the death", Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
	})
	app.sessionDeathWork.Close()

	if app.goSessionDeathWork(threadID, "probe", func() { starts.Add(100) }) {
		t.Fatal("death work was started after the join")
	}
	deathEvents(t, app, threadID, "token-dead")

	if got := app.sessionDeathWork.members(); got != 0 {
		t.Fatalf("%d death-work members after the join", got)
	}
	if draft, _, err := app.store.GetThreadDraft(threadID); err != nil || draft.Content != "" {
		t.Fatalf("draft = %q, %v; want nothing restored after the join", draft.Content, err)
	}
	if !app.triage.HasQueuedFlushItems(threadID) {
		t.Fatal("the queue was drained after the join")
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("starts = %d after the join, want none", got)
	}
}

// A death decides the thread's session only until something else does. A
// Stop, a rest, an archive, a start since the death, or a live replacement
// leaves the reconnect nothing to do, and its one attempt stays unspent for
// a later death.
func TestAStaleDeathDoesNotReconnect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		intervene  func(t *testing.T, app *App, threadID string)
		reconnects bool
	}{
		{name: "nothing since the death", reconnects: true},
		{name: "the person's Stop", intervene: func(t *testing.T, app *App, threadID string) {
			if err := app.StopSession(threadID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a rest", intervene: func(t *testing.T, app *App, threadID string) {
			var result ThreadAgentStop
			app.restThreadHere(context.Background(), threadID, userRestStop(), map[string]bool{}, &result)
			if len(result.Failures) > 0 {
				t.Fatalf("rest: %v", result.Failures)
			}
		}},
		{name: "an archive", intervene: func(t *testing.T, app *App, threadID string) {
			if _, err := app.ArchiveThread(threadID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a start since the death", intervene: func(t *testing.T, app *App, threadID string) {
			app.triage.MarkThreadActive(threadID, "token-replacement")
		}},
		{name: "a live replacement", intervene: func(t *testing.T, app *App, threadID string) {
			app.triage.MarkThreadActive(threadID, "token-replacement")
			app.sessionManager().put(threadID, session{Provider: string(provider.Claude), Token: "token-replacement"})
		}},
		{name: "a replacement that ran and was stopped", intervene: func(t *testing.T, app *App, threadID string) {
			app.triage.MarkThreadActive(threadID, "token-replacement")
			app.sessionManager().put(threadID, session{Provider: string(provider.Claude), Token: "token-replacement", Session: &closeRecordingSession{}})
			if err := app.StopSession(threadID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithTriage(t)
			threadID := reconnectableThread(t, app, "thread-stale-death")
			app.triage.MarkThreadActive(threadID, "token-dead")
			deathEpoch := app.triage.ThreadEpoch(threadID)
			if tc.intervene != nil {
				tc.intervene(t, app, threadID)
			}
			var starts atomic.Int32
			app.stopSessionFn = func(string) error { return nil }
			app.startSessionFn = func(string) error { starts.Add(1); return nil }

			app.attemptAutoReconnect(threadID, func() bool { return app.deathOwnsThread(threadID, "token-dead", deathEpoch) })

			if reconnected := starts.Load() == 1; reconnected != tc.reconnects {
				t.Fatalf("reconnected = %v, want %v", reconnected, tc.reconnects)
			}
			if unspent := app.markAutoReconnectAttempted(threadID); unspent == tc.reconnects {
				t.Fatalf("attempt unspent = %v after reconnected = %v", unspent, tc.reconnects)
			}
		})
	}
}

// A reconnect that shutdown refused, or that failed on the ended app
// context, is not a failure the person can act on, so it writes no error
// row. Any other failure does, including one joined with a refusal, a
// cancellation while the app runs, and a deadline.
func TestAutoReconnectRefusedByShutdownWritesNoErrorRow(t *testing.T) {
	t.Parallel()
	failure := errors.New("chdir /gone: no such file or directory")
	for _, tc := range []struct {
		name       string
		err        error
		appEnded   bool
		wantErrRow bool
	}{
		{name: "shutting down", err: fmt.Errorf("start session: %w", ErrShuttingDown)},
		{name: "refused, with nothing else joined", err: errors.Join(fmt.Errorf("start session: %w", ErrShuttingDown), nil)},
		{name: "app context ended", err: fmt.Errorf("start session: %w", context.Canceled), appEnded: true},
		{name: "refused and cancelled", err: errors.Join(ErrShuttingDown, context.Canceled), appEnded: true},
		{name: "cancelled while the app runs", err: fmt.Errorf("start session: %w", context.Canceled), wantErrRow: true},
		{name: "deadline after the app ended", err: fmt.Errorf("start session: %w", context.DeadlineExceeded), appEnded: true, wantErrRow: true},
		{name: "a failure joined with a cancellation", err: errors.Join(failure, context.Canceled), appEnded: true, wantErrRow: true},
		{name: "a failure joined with a refusal", err: errors.Join(fmt.Errorf("start session: %w", ErrShuttingDown), failure), wantErrRow: true},
		{name: "other failure", err: failure, wantErrRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			threadID := reconnectableThread(t, app, "thread-reconnect-refused")
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

			app.attemptAutoReconnect(threadID, func() bool { return true })

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

// The close hook supplies the result of a teardown's close of a session the
// registry held, after the real close ran. A stop of a thread with no
// session does not consult it.
func TestCloseHookAnswersAfterTheRealClose(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-close-hook")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected close failure")
	registered := &closeRecordingSession{}
	var calls, closedFirst atomic.Int32
	app.closeProviderSessionFn = func(string) error {
		calls.Add(1)
		closedFirst.Store(registered.closes.Load())
		return injected
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession with no session: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the close hook ran %d times for a thread with no session", got)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "tok", Session: registered})
	if err := app.StopSession(thread.ID); !errors.Is(err, injected) {
		t.Fatalf("StopSession = %v, want the hook's close failure", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the close hook ran %d times for a registered session, want 1", got)
	}
	if got := closedFirst.Load(); got != 1 {
		t.Fatalf("the real close had run %d times when the hook answered, want 1", got)
	}
}

// The harness's barrier between tests waits for the death work too, so a
// session a death's reconnect registers is in the list it returns and the
// reset stops it. The forward check is deterministic. Without the wait the
// test fails only when the barrier reads the registry before the reconnect
// registers, which is most runs and not every one, and it cannot show that
// no other work the barrier does not wait for registers a session.
func TestHarnessBarrierNamesTheSessionADeathReconnected(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := reconnectableThread(t, app, "thread-harness-barrier")
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(id string) error {
		return app.registerStartedSession(id, session{Provider: string(provider.Claude), Token: "token-reconnected"})
	}
	deathEvents(t, app, threadID, "token-dead")

	if live := (&harnessHost{app: app}).AwaitSessionEndWork(); !slices.Contains(live, threadID) {
		t.Fatalf("barrier listed %v, want the reconnected %s", live, threadID)
	}
}

// An open on a dedicated Codex process keeps its own answer while the app
// runs. Once the app's end closed the process, shutdown refused the open,
// whether it succeeded (its session is closed) or failed on the closed
// process; only a failure to close the opened session is a failure.
func TestAnOpenAfterTheAppEndedIsRefusedByShutdown(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	appCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.appCtx = appCtx
	broken := errors.New("codex: connection closed")
	closeFailed := errors.New("injected close failure")
	for _, tc := range []struct {
		name         string
		appEnded     bool
		openErr      error
		closeErr     error
		wantErr      error
		wantRefusal  bool
		wantSessions int32
	}{
		{name: "opened while the app runs"},
		{name: "failed while the app runs", openErr: broken, wantErr: broken},
		{name: "opened after the app ended", appEnded: true, wantErr: ErrShuttingDown, wantRefusal: true, wantSessions: 1},
		{name: "failed on the closed process", appEnded: true, openErr: broken, wantErr: ErrShuttingDown, wantRefusal: true},
		{name: "opened, and its close failed", appEnded: true, closeErr: closeFailed, wantErr: closeFailed, wantSessions: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32
			disarm := func() bool { return !tc.appEnded }
			err := refuseOpenAfterAppEnd(disarm, "thread-dedicated", tc.openErr, func() error { closes.Add(1); return tc.closeErr })
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("open = %v, want %v", err, tc.wantErr)
			}
			if got := err != nil && app.refusedByShutdown(err); got != tc.wantRefusal {
				t.Fatalf("refused by shutdown = %v for %v, want %v", got, err, tc.wantRefusal)
			}
			if got := closes.Load(); got != tc.wantSessions {
				t.Fatalf("opened session closed %d times, want %d", got, tc.wantSessions)
			}
		})
	}
}

// The close armed for a process no pool owns fires when the app's life
// ends before the owner takes it, and not after the owner did.
func TestCloseWhenAppEndsFiresOnlyBeforeDisarm(t *testing.T) {
	t.Parallel()
	for _, endFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("app ends first %v", endFirst), func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			appCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			app.appCtx = appCtx
			closed := make(chan struct{})
			conn := &closeRecordingSession{onClose: func() { close(closed) }}
			disarm := app.closeWhenAppEnds(conn, "test process")
			wantCloses := int32(0)
			if endFirst {
				cancel()
				waitClosed(t, closed, "the close armed on the app context")
				wantCloses = 1
			}
			if stopped := disarm(); stopped == endFirst {
				t.Fatalf("disarm = %v with the app ended first %v", stopped, endFirst)
			}
			cancel()
			if got := conn.closes.Load(); got != wantCloses {
				t.Fatalf("closed %d times, want %d", got, wantCloses)
			}
		})
	}
}
