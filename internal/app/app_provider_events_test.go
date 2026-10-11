package app

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// armProcessSchedule gives the thread a ScheduleWakeup timer and a one-shot
// cron job, both state of the live CLI process.
func armProcessSchedule(t *testing.T, router *triage.Router, threadID string) {
	t.Helper()
	wakeup, _ := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: time.Now().Add(30 * time.Minute).UnixMilli()})
	jobs, _ := json.Marshal(provider.ScheduledJobsMeta{Jobs: []provider.ScheduledJob{{ID: "once", Cron: "0 9 * * 1", FireByUnixMs: time.Now().Add(6 * 24 * time.Hour).UnixMilli()}}})
	for _, evt := range []provider.ProviderEvent{
		{Kind: provider.EventSessionWakeup, ThreadID: threadID, Meta: wakeup},
		{Kind: provider.EventScheduledJobs, ThreadID: threadID, Meta: jobs},
	} {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("arm %s: %v", evt.Kind, err)
		}
	}
	if _, ok := router.PendingScheduledWorkAt(threadID); !ok {
		t.Fatal("the armed schedule did not register")
	}
}

// TestUnregisterSessionRemovesSessionForMatchingToken covers the normal
// disconnect path: the session stored with the same token is dropped.
func TestUnregisterSessionRemovesSessionForMatchingToken(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-unregister")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "token-live",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventModelFallback,
		ThreadID: thread.ID,
		ItemID:   "model-fallback:req-unregister",
		Meta:     json.RawMessage(`{"fallbackModel":"claude-opus-4-8"}`),
	}); err != nil {
		t.Fatalf("seed model fallback: %v", err)
	}
	armProcessSchedule(t, app.triage, thread.ID)

	app.unregisterSession(thread.ID, "token-live")

	_, present := app.sessionManager().get(thread.ID)
	if present {
		t.Fatal("expected session to be removed after unregisterSession")
	}
	if got := app.triage.LiveStateSnapshotForThread(thread.ID).EffectiveModel; got != "" {
		t.Fatalf("effective model after unregister = %q", got)
	}
	// The wakeup timer and the job died with the process.
	if at, ok := app.triage.PendingWakeupAt(thread.ID); ok {
		t.Fatalf("schedule after the process exited = %v", at)
	}
	if app.triage.AnyUnfinishedWork(time.Minute) {
		t.Fatal("an exited process's schedule still blocks updates")
	}
}

// TestUnregisterSessionKeepsSessionWhenTokenIsStale protects against the
// reconnect-race: if an older goroutine signals a disconnect AFTER a
// replacement session has already been installed, we must NOT remove the
// replacement. Matches the behavior asserted in
// TestStaleSessionDisconnectDoesNotRemoveReplacement for the broader path.
func TestUnregisterSessionKeepsSessionWhenTokenIsStale(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-unregister-stale")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "token-current",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventModelFallback,
		ThreadID: thread.ID,
		ItemID:   "model-fallback:req-stale-unregister",
		Meta:     json.RawMessage(`{"fallbackModel":"claude-opus-4-8"}`),
	}); err != nil {
		t.Fatalf("seed model fallback: %v", err)
	}
	armProcessSchedule(t, app.triage, thread.ID)

	app.unregisterSession(thread.ID, "token-previous")

	current, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("replacement session was dropped on stale unregister")
	}
	if current.Token != "token-current" {
		t.Fatalf("token = %q, want token-current preserved", current.Token)
	}
	if got := app.triage.LiveStateSnapshotForThread(thread.ID).EffectiveModel; got != "claude-opus-4-8" {
		t.Fatalf("effective model after stale unregister = %q", got)
	}
	if _, ok := app.triage.PendingScheduledWorkAt(thread.ID); !ok {
		t.Fatal("a stale exit dropped the replacement session's schedule")
	}
}

// TestUnregisterSessionWithNoEntryIsSafe confirms we can safely invoke the
// unregister path even if the entry is already gone (e.g. due to an earlier
// StopSession). It must not panic.
func TestUnregisterSessionWithNoEntryIsSafe(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	app.unregisterSession("thread-absent", "any-token")

	if len(app.sessionManager().runtime.Snapshot()) != 0 {
		t.Fatalf("sessions = %v, want empty", app.sessionManager().runtime.Snapshot())
	}
}

// TestSessionEventHandlerDisconnectUnregistersSession verifies the
// disconnect path inside sessionEventHandler: the matching-token session is
// cleaned up when a "disconnected" session_status event arrives.
func TestSessionEventHandlerDisconnectUnregistersSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-disconnect-handler")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "token-disconnect",
	})

	handler := app.sessionEventHandler(thread.ID, "token-disconnect", "")
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})
	waitProviderEvents(t, app, thread.ID)

	_, present := app.sessionManager().get(thread.ID)
	if present {
		t.Fatal("session not removed after disconnect event")
	}
}

// TestSessionEventHandlerNonDisconnectStatusPreservesSession makes sure that
// a non-disconnect session_status (e.g. "ready", "error") does NOT remove
// the session. Only the literal "disconnected" content triggers cleanup.
func TestSessionEventHandlerNonDisconnectStatusPreservesSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-status-preserve")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "token-ok",
	})

	handler := app.sessionEventHandler(thread.ID, "token-ok", "")
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "ready",
		Timestamp: time.Now(),
	})

	waitProviderEvents(t, app, thread.ID)

	_, present := app.sessionManager().get(thread.ID)
	if !present {
		t.Fatal("session wrongly removed on non-disconnect status event")
	}
}

// TestSessionEventHandlerAutoReconnectsAfterAbnormalDeath verifies the
// recovery hook fires when a session sees the "error" → "disconnected"
// pair (an abnormal exit) and the thread has a stored SessionRef. The
// auto-reconnect is the path that turns SIGTERM / clean-exit-0 / OOM-kill
// into a silently-recovered session without user action.
func TestSessionEventHandlerAutoReconnectsAfterAbnormalDeath(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto-reconnect")
	thread.SessionRef = "claude-resume-abc"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "token-dead",
	})

	startCalls := make(chan string, 4)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(threadID string) error {
		startCalls <- threadID
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-dead", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	waitProviderEvents(t, app, thread.ID)

	select {
	case got := <-startCalls:
		if got != thread.ID {
			t.Fatalf("startSessionFn invoked for %q, want %q", got, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-reconnect startSession call")
	}
}

// TestSessionEventHandlerNoAutoReconnectWithoutDeathSignal pins the
// intentional-stop carve-out: a "disconnected" event that arrived without
// a preceding "error" came from us closing the session and must NOT
// resurrect it. Without this gate, calling StopSession on a thread would
// instantly restart its provider.
func TestSessionEventHandlerNoAutoReconnectWithoutDeathSignal(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-clean-stop")
	thread.SessionRef = "claude-resume-clean"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "token-clean",
	})

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-clean", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	waitProviderEvents(t, app, thread.ID)

	select {
	case <-startCalled:
		t.Fatal("startSessionFn called on clean disconnect; auto-reconnect should be gated on prior error event")
	case <-time.After(150 * time.Millisecond):
		// expected — no auto-reconnect for a clean stop
	}
}

// TestSessionEventHandlerNoAutoReconnectWithoutSessionRef confirms the
// SessionRef gate: a death that hits before the provider published a
// resume cursor cannot be auto-recovered (--resume needs a target), so we
// leave the banner up and let the user decide.
func TestSessionEventHandlerNoAutoReconnectWithoutSessionRef(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-sessionref")
	// SessionRef intentionally empty
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "token-early",
	})

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-early", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	waitProviderEvents(t, app, thread.ID)

	select {
	case <-startCalled:
		t.Fatal("startSessionFn called despite empty SessionRef; auto-reconnect should skip when there is nothing to --resume against")
	case <-time.After(150 * time.Millisecond):
		// expected
	}
}

func TestAutoReconnectSkipsWorkflowThreads(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-workflow-no-reconnect")
	thread.Mode = "workflow"
	thread.SessionRef = "workflow-resume-ref"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := false
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		started = true
		return nil
	}
	app.attemptAutoReconnect(thread.ID)
	if started {
		t.Fatal("workflow thread used generic auto-reconnect")
	}
}

// TestAutoReconnectRearmsOnlyAfterAServedTurn takes two deaths' reconnect
// decisions with one event from the replacement session between them,
// delivered through the public event handler. The second death earns a
// reconnect only when that event proves the replacement served a turn, for
// each provider in the form it reports one. Everything else, including
// init, leaves the single attempt spent, so a session that dies before
// serving cannot reconnect forever.
func TestAutoReconnectRearmsOnlyAfterAServedTurn(t *testing.T) {
	t.Parallel()
	result := func(meta provider.WireTurnCompleteMeta) provider.ProviderEvent {
		return provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &meta}
	}
	failed := result(provider.WireTurnCompleteMeta{StopReason: "end_turn"})
	failed.Failure = &provider.FailureMeta{Class: provider.FailureTransient}
	subagent := result(provider.WireTurnCompleteMeta{StopReason: "end_turn"})
	subagent.ParentToolUseID = "toolu_parent"
	cases := []struct {
		name     string
		provider provider.ProviderKind
		served   *provider.ProviderEvent
		rearmed  bool
	}{
		{name: "claude nothing", provider: provider.Claude},
		{name: "claude init", provider: provider.Claude, served: &provider.ProviderEvent{Kind: provider.EventInit}},
		{name: "claude result", provider: provider.Claude, served: ptr(result(provider.WireTurnCompleteMeta{StopReason: "end_turn"})), rearmed: true},
		{name: "claude interrupted result", provider: provider.Claude, served: ptr(result(provider.WireTurnCompleteMeta{Aborted: true})), rearmed: true},
		{name: "claude error result", provider: provider.Claude, served: ptr(result(provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "boom"}))},
		{name: "claude failed result", provider: provider.Claude, served: &failed},
		{name: "claude subagent result", provider: provider.Claude, served: &subagent},
		{name: "claude soft round close", provider: provider.Claude, served: &provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}},
		{name: "codex turn start", provider: provider.Codex, served: &provider.ProviderEvent{Kind: provider.EventTurnStart}, rearmed: true},
		{name: "codex completed turn", provider: provider.Codex, served: ptr(result(provider.WireTurnCompleteMeta{StopReason: "end_turn"})), rearmed: true},
		{name: "codex failed turn", provider: provider.Codex, served: ptr(result(provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "boom"}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			thread := testThread("thread-rearm")
			thread.SessionRef = "resume-rearm"
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread() error = %v", err)
			}
			startCalls := make(chan string, 8)
			app.stopSessionFn = func(string) error { return nil }
			app.startSessionFn = func(threadID string) error {
				startCalls <- threadID
				return nil
			}
			// attemptAutoReconnect is the hook the handler runs after each
			// death (TestSessionEventHandlerAutoReconnectsAfterAbnormalDeath).
			// Called directly, its reconnect has started the replacement or
			// declined by the time it returns.
			reconnected := func() bool {
				app.attemptAutoReconnect(thread.ID)
				select {
				case <-startCalls:
					return true
				default:
					return false
				}
			}

			if !reconnected() {
				t.Fatal("first death did not reconnect")
			}
			app.sessionManager().put(thread.ID, session{Provider: string(tc.provider), Token: "tok-2"})
			if tc.served != nil {
				evt := *tc.served
				evt.ThreadID = thread.ID
				evt.Timestamp = time.Now()
				app.sessionEventHandler(thread.ID, "tok-2", string(tc.provider))(evt)
				waitProviderEvents(t, app, thread.ID)
			}
			if got := reconnected(); got != tc.rearmed {
				t.Fatalf("second death reconnected = %v, want %v", got, tc.rearmed)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestAttemptAutoReconnectSkippedDuringShutdown pins the shutdown-race
// safety check: a death arriving concurrently with a.Stop() must not
// resurrect the provider after the app has begun teardown.
func TestAttemptAutoReconnectSkippedDuringShutdown(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown")
	thread.SessionRef = "claude-resume-shutdown"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	app.shuttingDown.Store(true)

	handler := app.sessionEventHandler(thread.ID, "tok-shutdown", string(provider.Claude))
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "tok-shutdown"})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	waitProviderEvents(t, app, thread.ID)

	select {
	case <-startCalled:
		t.Fatal("auto-reconnect fired during shutdown; shuttingDown gate failed")
	case <-time.After(150 * time.Millisecond):
		// expected — shutdown gated it
	}
}

// TestReconnectSessionSingleFlightSecondCallNoOps pins the gate that
// stops a concurrent ReconnectSession from yanking the in-flight start.
// The second caller returns nil without invoking stop/start.
func TestReconnectSessionSingleFlightSecondCallNoOps(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	var calls []string
	var callsMu sync.Mutex

	startSignal := make(chan struct{})
	startBlock := make(chan struct{})

	app.stopSessionFn = func(threadID string) error {
		callsMu.Lock()
		calls = append(calls, "stop:"+threadID)
		callsMu.Unlock()
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		callsMu.Lock()
		calls = append(calls, "start:"+threadID)
		callsMu.Unlock()
		close(startSignal)
		<-startBlock
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- app.ReconnectSession("thread-sf")
	}()

	<-startSignal // first call is inside start now, holding the gate

	// Second concurrent call must return nil without invoking stop/start.
	if err := app.ReconnectSession("thread-sf"); err != nil {
		t.Fatalf("second ReconnectSession returned err=%v, want nil", err)
	}

	close(startBlock)
	if err := <-done; err != nil {
		t.Fatalf("first ReconnectSession returned err=%v", err)
	}

	callsMu.Lock()
	got := append([]string(nil), calls...)
	callsMu.Unlock()
	want := []string{"stop:thread-sf", "start:thread-sf"}
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v (second call must NOT add a second stop/start pair)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
