package main

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// The trim RPC's four outcomes, and the one emission it is allowed to make.
// The gates are the feature: a GC pause is invisible on an idle window and a
// visible hitch mid-stream, so "skipped-active-turn" not emitting is as
// load-bearing as "requested" emitting.
func TestRequestWebviewMemoryTrim(t *testing.T) {
	newApp := func(t *testing.T) (*App, *[]string) {
		app := newTestAppWithStore(t)
		var emitted []string
		app.testEmitHook = func(name string, data any) {
			emitted = append(emitted, name)
		}
		return app, &emitted
	}
	trimEmits := func(emitted []string) int {
		n := 0
		for _, name := range emitted {
			if name == string(eventchan.WebviewTrim) {
				n++
			}
		}
		return n
	}

	t.Run("idle app emits the directive once and rate-limits the second ask", func(t *testing.T) {
		app, emitted := newApp(t)

		// inputSinceLastTrim false on purpose: the first trim since boot
		// passes the activity gate regardless — boot render churn is work.
		outcome, err := app.RequestWebviewMemoryTrim(false)
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "requested" {
			t.Fatalf("outcome = %q, want %q", outcome, "requested")
		}
		if got := trimEmits(*emitted); got != 1 {
			t.Fatalf("webview:trim emissions = %d, want 1", got)
		}

		outcome, err = app.RequestWebviewMemoryTrim(true)
		if err != nil {
			t.Fatalf("second RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "skipped-recent" {
			t.Fatalf("second outcome = %q, want %q", outcome, "skipped-recent")
		}
		if got := trimEmits(*emitted); got != 1 {
			t.Fatalf("webview:trim emissions after rate-limited ask = %d, want 1", got)
		}
	})

	t.Run("no input and no turn since the last trim refuses the metronome", func(t *testing.T) {
		// The overnight failure shape: idleness persists, nothing ran, and
		// the reattempt cadence used to force a ~50ms GC every floor
		// interval anyway — 717 in one sitting (2026-08-26).
		app, emitted := newApp(t)

		if outcome, _ := app.RequestWebviewMemoryTrim(true); outcome != "requested" {
			t.Fatalf("first outcome = %q, want %q", outcome, "requested")
		}

		agesAgo := func() {
			app.webviewTrimLastUnixNano.Store(
				time.Now().Add(-webviewTrimMinInterval - time.Second).UnixNano())
		}

		agesAgo()
		outcome, err := app.RequestWebviewMemoryTrim(false)
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "skipped-no-activity" {
			t.Fatalf("quiet outcome = %q, want %q", outcome, "skipped-no-activity")
		}
		if got := trimEmits(*emitted); got != 1 {
			t.Fatalf("webview:trim emissions after quiet ask = %d, want 1", got)
		}

		// A provider turn ran since: the backend's half of the gate opens.
		app.turnActivityUnixNano.Store(time.Now().UnixNano())
		if outcome, _ := app.RequestWebviewMemoryTrim(false); outcome != "requested" {
			t.Fatalf("post-turn outcome = %q, want %q", outcome, "requested")
		}
		if got := trimEmits(*emitted); got != 2 {
			t.Fatalf("webview:trim emissions after a turn = %d, want 2", got)
		}

		// Input landed since: the caller's half opens it on its own.
		agesAgo()
		if outcome, _ := app.RequestWebviewMemoryTrim(true); outcome != "requested" {
			t.Fatalf("post-input outcome = %q, want %q", outcome, "requested")
		}
		if got := trimEmits(*emitted); got != 3 {
			t.Fatalf("webview:trim emissions after input = %d, want 3", got)
		}
	})

	t.Run("an open provider turn refuses the trim entirely", func(t *testing.T) {
		app, emitted := newApp(t)

		// Aged past the recent-wire window: this subtest exercises the
		// activeTurns arm alone.
		live := newSessionLiveness(time.Now().Add(-time.Minute))
		live.activeTurns.Add(1)
		app.mu.Lock()
		app.sessions["thread-1"] = session{liveness: live}
		app.mu.Unlock()

		outcome, err := app.RequestWebviewMemoryTrim(true)
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "skipped-active-turn" {
			t.Fatalf("outcome = %q, want %q", outcome, "skipped-active-turn")
		}
		if got := trimEmits(*emitted); got != 0 {
			t.Fatalf("webview:trim emissions during an active turn = %d, want 0", got)
		}

		// The turn ending is what unblocks the next ask — no rate-limit
		// debt accrues from a refused one.
		live.activeTurns.Add(-1)
		outcome, err = app.RequestWebviewMemoryTrim(true)
		if err != nil {
			t.Fatalf("post-turn RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "requested" {
			t.Fatalf("post-turn outcome = %q, want %q", outcome, "requested")
		}
		if got := trimEmits(*emitted); got != 1 {
			t.Fatalf("webview:trim emissions after the turn ended = %d, want 1", got)
		}
	})

	t.Run("a Claude turn refuses the trim through triage round state", func(t *testing.T) {
		// The 2026-08-27 live defect: Claude sessions never emit
		// EventTurnStart, so the activeTurns counter is permanently zero
		// for them and the old gate let every idle trim land mid-turn as
		// a 60-130ms GC stall. The router's wire-round state is the
		// provider-agnostic arm that must catch it.
		app, emitted := newApp(t)
		app.ensureTriageRouter()

		thread := testThread("thread-trim-claude")
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread() error = %v", err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  thread.ID,
			TurnID:    "turn-trim",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn start: %v", err)
		}

		outcome, err := app.RequestWebviewMemoryTrim(true)
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "skipped-active-turn" {
			t.Fatalf("mid-round outcome = %q, want %q", outcome, "skipped-active-turn")
		}
		if got := trimEmits(*emitted); got != 0 {
			t.Fatalf("webview:trim emissions mid-round = %d, want 0", got)
		}

		if err := app.triage.Handle(provider.ProviderEvent{
			Kind:         provider.EventTurnComplete,
			ThreadID:     thread.ID,
			TurnID:       "turn-trim",
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
			Timestamp:    time.Now(),
		}); err != nil {
			t.Fatalf("turn complete: %v", err)
		}
		if outcome, _ := app.RequestWebviewMemoryTrim(true); outcome != "requested" {
			t.Fatalf("post-round outcome = %q, want %q", outcome, "requested")
		}
	})

	t.Run("recent wire activity refuses the trim without any open turn", func(t *testing.T) {
		// The sidechain tail: a backgrounded subagent keeps streaming
		// after the parent round soft-closes. No turn-state predicate
		// covers it; the lastActivity stamp does.
		app, emitted := newApp(t)

		live := newSessionLiveness(time.Now())
		app.mu.Lock()
		app.sessions["thread-1"] = session{liveness: live}
		app.mu.Unlock()

		outcome, err := app.RequestWebviewMemoryTrim(true)
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "skipped-active-turn" {
			t.Fatalf("recent-wire outcome = %q, want %q", outcome, "skipped-active-turn")
		}
		if got := trimEmits(*emitted); got != 0 {
			t.Fatalf("webview:trim emissions with recent wire = %d, want 0", got)
		}

		// Quiet wire past the window: the gate opens.
		live.lastActivityUnixNano.Store(
			time.Now().Add(-webviewTrimRecentWireWindow - time.Second).UnixNano())
		if outcome, _ := app.RequestWebviewMemoryTrim(true); outcome != "requested" {
			t.Fatalf("quiet-wire outcome = %q, want %q", outcome, "requested")
		}
	})
}
