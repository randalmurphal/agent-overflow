package app

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/mcpstatus"
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

// TestUnregisterSessionEndsTheScheduleOfATakenSession covers every host
// stop (a transfer, an idle reap, StopSession, a replacement start): the
// stop takes the session out of the registry before closing it, so the
// session's "disconnected" finds no entry, and its schedule still ends.
func TestUnregisterSessionEndsTheScheduleOfATakenSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-unregister-taken")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "token-taken"})
	armProcessSchedule(t, app.triage, thread.ID)
	recurring, _ := json.Marshal(provider.ScheduledJobsMeta{Jobs: []provider.ScheduledJob{{ID: "loop", Cron: "*/5 * * * *", Recurring: true, FireByUnixMs: time.Now().Add(10 * time.Minute).UnixMilli()}}})
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventScheduledJobs, ThreadID: thread.ID, Meta: recurring}); err != nil {
		t.Fatalf("arm recurring job: %v", err)
	}
	if _, ok := app.sessionManager().take(thread.ID); !ok {
		t.Fatal("take() found no session")
	}

	app.unregisterSession(thread.ID, "token-taken")

	if at, ok := app.triage.PendingWakeupAt(thread.ID); ok {
		t.Fatalf("schedule after a stop ended the process = %v", at)
	}
	if jobs, err := app.ListThreadRecurringJobs(thread.ID); err != nil || len(jobs) != 0 {
		t.Fatalf("recurring jobs after a stop ended the process = %v, %v", jobs, err)
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

	handler := startedSessionEventHandler(app, thread.ID, "token-disconnect", "")
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

	handler := startedSessionEventHandler(app, thread.ID, "token-ok", "")
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

	handler := startedSessionEventHandler(app, thread.ID, "token-dead", string(provider.Claude))
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

	handler := startedSessionEventHandler(app, thread.ID, "token-clean", string(provider.Claude))
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

	handler := startedSessionEventHandler(app, thread.ID, "token-early", string(provider.Claude))
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

// TestAutoReconnectLeavesALiveReplacementAlone: a send after the death
// already started a replacement session before the reconnect reached the
// thread lock. Stopping it would end its turn and drop its queue, so the
// reconnect does nothing; with no live session it reconnects.
func TestAutoReconnectLeavesALiveReplacementAlone(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			thread := testThread(fmt.Sprintf("thread-reconnect-live-%v", live))
			thread.SessionRef = "claude-resume-live"
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread() error = %v", err)
			}
			if live {
				app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "token-replacement"})
			}
			stops, starts := 0, 0
			app.stopSessionFn = func(string) error { stops++; return nil }
			app.startSessionFn = func(string) error { starts++; return nil }

			app.attemptAutoReconnect(thread.ID)

			want := 1
			if live {
				want = 0
			}
			if stops != want || starts != want {
				t.Fatalf("stops=%d starts=%d, want %d each", stops, starts, want)
			}
		})
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
				startedSessionEventHandler(app, thread.ID, "tok-2", string(tc.provider))(evt)
				waitProviderEvents(t, app, thread.ID)
			}
			// The replacement's death unregisters it, if the event above
			// did not already, before its reconnect runs: the slot alone
			// decides, not the live-session guard.
			app.sessionManager().unregister(thread.ID, "tok-2")
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

	handler := startedSessionEventHandler(app, thread.ID, "tok-shutdown", string(provider.Claude))
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

// A frame of a session the thread is no longer committed to is the
// replacement's business in no way: the host does nothing with it, before
// or after triage refuses it. Here the dead session's turn, death and
// disconnect are handled after the replacement start committed the thread
// but before the replacement registered, the window a stalled worker hands
// them over in. None of them may clear what the replacement armed, reach a
// turn observer or start an auto-reconnect over it.
func TestRetiredSessionFramesDoNotActOnTheReplacement(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("thread-retired-frames")
	thread.Provider = string(provider.Claude)
	thread.SessionRef = "claude-resume-retired"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	starts := make(chan string, 4)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(threadID string) error {
		starts <- threadID
		return nil
	}
	var observed sync.Mutex
	var kinds []provider.EventKind
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		observed.Lock()
		kinds = append(kinds, evt.Kind)
		observed.Unlock()
	})
	t.Cleanup(unsubscribe)

	dead := startedSessionEventHandler(app, thread.ID, "token-retired", string(provider.Claude))
	app.triage.MarkThreadActive(thread.ID, "token-replacement")
	armProcessSchedule(t, app.triage, thread.ID)

	for _, evt := range []provider.ProviderEvent{
		{Kind: provider.EventTurnComplete, ThreadID: thread.ID, TurnComplete: &provider.WireTurnCompleteMeta{}},
		{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "error"},
		{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "disconnected"},
	} {
		evt.Timestamp = time.Now()
		dead(evt)
	}
	waitProviderEvents(t, app, thread.ID)

	if _, ok := app.triage.PendingWakeupAt(thread.ID); !ok {
		t.Fatal("the retired session's disconnect cleared the replacement's schedule")
	}
	observed.Lock()
	got := append([]provider.EventKind(nil), kinds...)
	observed.Unlock()
	if len(got) != 0 {
		t.Fatalf("turn observers saw the retired session's frames: %v", got)
	}
	select {
	case threadID := <-starts:
		t.Fatalf("the retired session's death started an auto-reconnect for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

// The verifier's probe, kept as it found the gap: session A's frame is stuck
// inside triage when the replacement commits the thread. Once it unsticks,
// none of the host's effects after triage may run for it.
func TestInFlightRetiredFrameDoesNotReachTurnObservers(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("thread-probe-inflight")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	inHandle := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	app.triage.SetEventHook(func(evt provider.ProviderEvent) {
		if evt.SessionToken == "token-a" && evt.Kind == provider.EventTurnComplete {
			once.Do(func() { close(inHandle); <-release })
		}
	})
	var mu sync.Mutex
	var seen []provider.EventKind
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		mu.Lock()
		seen = append(seen, evt.Kind)
		mu.Unlock()
	})
	t.Cleanup(unsubscribe)

	dead := startedSessionEventHandler(app, thread.ID, "token-a", string(provider.Claude))
	dead(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: thread.ID, TurnComplete: &provider.WireTurnCompleteMeta{}, Timestamp: time.Now()})
	awaitHeldInHandle(t, inHandle)
	mu.Lock()
	seen = nil
	mu.Unlock()
	app.triage.MarkThreadActive(thread.ID, "token-b")
	close(release)
	waitProviderEvents(t, app, thread.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("turn observers saw the retired session's in-flight frame after the replacement committed: %v", seen)
	}
}

// The PR's race in one process: A's worker is stuck inside triage on one
// frame while A's ScheduleWakeup ack queues behind it, and the replacement
// commits. The ack reaches triage only after the commit, carrying A's token
// from the read-loop callback, so the replacement holds no schedule.
func TestFrameQueuedBehindAStuckHandlerNeverArmsTheReplacement(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("thread-stuck-worker")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	inHandle := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	app.triage.SetEventHook(func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventTurnComplete {
			once.Do(func() { close(inHandle); <-release })
		}
	})
	dead := startedSessionEventHandler(app, thread.ID, "token-a", string(provider.Claude))
	dead(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: thread.ID, TurnComplete: &provider.WireTurnCompleteMeta{}, Timestamp: time.Now()})
	awaitHeldInHandle(t, inHandle)
	wakeup, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: time.Now().Add(30 * time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	dead(provider.ProviderEvent{Kind: provider.EventSessionWakeup, ThreadID: thread.ID, Meta: wakeup, Timestamp: time.Now()})
	app.triage.MarkThreadActive(thread.ID, "token-b")
	close(release)
	waitProviderEvents(t, app, thread.ID)
	if at, ok := app.triage.PendingWakeupAt(thread.ID); ok {
		t.Fatalf("the replacement holds the retired session's wakeup until %s", at)
	}
}

// awaitHeldInHandle waits for a test's event hook to hold its frame inside
// triage, and fails rather than hangs when the frame never arrives there.
func awaitHeldInHandle(t *testing.T, inHandle <-chan struct{}) {
	t.Helper()
	select {
	case <-inHandle:
	case <-time.After(10 * time.Second):
		t.Fatal("the frame never reached triage's event hook")
	}
}

// An init already inside triage when a replacement commits does not feed
// the MCP status cache: the host's work on it waits for triage's answer.
func TestInFlightRetiredInitDoesNotFeedTheMCPStatusCache(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("thread-inflight-init")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	init, err := json.Marshal(provider.SessionInfo{MCPServers: []provider.MCPServer{{Name: "inflight-server", Status: "connected"}}})
	if err != nil {
		t.Fatal(err)
	}
	inHandle := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	app.triage.SetEventHook(func(evt provider.ProviderEvent) {
		if evt.SessionToken == "token-a" && evt.Kind == provider.EventInit {
			once.Do(func() { close(inHandle); <-release })
		}
	})
	dead := startedSessionEventHandler(app, thread.ID, "token-a", string(provider.Claude))
	dead(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: thread.ID, Meta: init, Timestamp: time.Now()})
	awaitHeldInHandle(t, inHandle)
	app.triage.MarkThreadActive(thread.ID, "token-b")
	close(release)
	waitProviderEvents(t, app, thread.ID)
	if cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "inflight-server"}); ok {
		t.Fatalf("a retired session's in-flight init fed the MCP status cache: %+v", cached)
	}
}

// A retired session's disconnect does not retire the thread's event queue,
// whether or not the replacement has queued a frame yet: the queue is the
// replacement's, and only the replacement's own end marks it.
func TestRetiredDisconnectKeepsTheReplacementsQueue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// replacementFrame is queued by the replacement behind the
		// retired session's disconnect, or nil when the replacement has
		// queued nothing yet (a Claude session emits nothing until its
		// first send).
		replacementFrame *provider.ProviderEvent
	}{
		{"after the replacement queued a frame", &provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "ready"}},
		{"before the replacement queued a frame", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithTriage(t)
			thread := testThread("thread-retired-queue")
			thread.Provider = string(provider.Claude)
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			dead := startedSessionEventHandler(app, thread.ID, "token-a", string(provider.Claude))
			if err := app.providerEvents.stall(thread.ID); err != nil {
				t.Fatal(err)
			}
			dead(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "disconnected", Timestamp: time.Now()})
			live := startedSessionEventHandler(app, thread.ID, "token-b", string(provider.Claude))
			if c.replacementFrame != nil {
				frame := *c.replacementFrame
				frame.ThreadID, frame.Timestamp = thread.ID, time.Now()
				live(frame)
			}
			if err := app.providerEvents.releaseStall(thread.ID); err != nil {
				t.Fatal(err)
			}
			waitProviderEvents(t, app, thread.ID)
			waitForCondition(t, "the worker goes idle", func() bool {
				q := app.providerEvents.lookup(thread.ID)
				if q == nil {
					return true
				}
				q.mu.Lock()
				defer q.mu.Unlock()
				return !q.running
			})
			if app.providerEvents.lookup(thread.ID) == nil {
				t.Fatal("the retired session's disconnect retired the replacement's queue")
			}

			// The replacement's frames are handled on its queue.
			wakeup, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: time.Now().Add(30 * time.Minute).UnixMilli()})
			if err != nil {
				t.Fatal(err)
			}
			live(provider.ProviderEvent{Kind: provider.EventSessionWakeup, ThreadID: thread.ID, Meta: wakeup, Timestamp: time.Now()})
			waitProviderEvents(t, app, thread.ID)
			if _, ok := app.triage.PendingWakeupAt(thread.ID); !ok {
				t.Fatal("the replacement's frame was not handled")
			}

			// The replacement's own end still retires it.
			live(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "disconnected", Timestamp: time.Now()})
			waitProviderEvents(t, app, thread.ID)
			waitForCondition(t, "the replacement's disconnect retires the queue", func() bool {
				return app.providerEvents.lookup(thread.ID) == nil
			})
		})
	}
}

// The host's own work before triage is gated on the same answer: a retired
// session's init does not overwrite the live session's MCP status.
func TestRetiredSessionInitDoesNotFeedTheMCPStatusCache(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("thread-retired-init")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	init, err := json.Marshal(provider.SessionInfo{MCPServers: []provider.MCPServer{{Name: "retired-server", Status: "connected"}}})
	if err != nil {
		t.Fatal(err)
	}
	dead := startedSessionEventHandler(app, thread.ID, "token-a", string(provider.Claude))
	app.triage.MarkThreadActive(thread.ID, "token-b")
	dead(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: thread.ID, Meta: init, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	if cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "retired-server"}); ok {
		t.Fatalf("a retired session's init fed the MCP status cache: %+v", cached)
	}
}
