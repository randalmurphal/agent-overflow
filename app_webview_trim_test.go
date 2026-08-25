package main

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// The trim RPC's three outcomes, and the one emission it is allowed to make.
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

		outcome, err := app.RequestWebviewMemoryTrim()
		if err != nil {
			t.Fatalf("RequestWebviewMemoryTrim() error = %v", err)
		}
		if outcome != "requested" {
			t.Fatalf("outcome = %q, want %q", outcome, "requested")
		}
		if got := trimEmits(*emitted); got != 1 {
			t.Fatalf("webview:trim emissions = %d, want 1", got)
		}

		outcome, err = app.RequestWebviewMemoryTrim()
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

	t.Run("an open provider turn refuses the trim entirely", func(t *testing.T) {
		app, emitted := newApp(t)

		live := newSessionLiveness(time.Now())
		live.activeTurns.Add(1)
		app.mu.Lock()
		app.sessions["thread-1"] = session{liveness: live}
		app.mu.Unlock()

		outcome, err := app.RequestWebviewMemoryTrim()
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
		outcome, err = app.RequestWebviewMemoryTrim()
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
}
