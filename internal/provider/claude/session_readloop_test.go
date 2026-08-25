package claude

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestReadLoopDispatchesTextDelta(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Text content reaches the read loop via stream_event envelopes
	// (the CLI always runs with --include-partial-messages). The
	// coalesced `assistant` envelope's text blocks are intentionally
	// skipped by the parser to avoid doubling the summary.
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"streaming text"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTextDelta)
	}
	if evt.Content != "streaming text" {
		t.Errorf("content: got %q, want %q", evt.Content, "streaming text")
	}
}

func TestReadLoopDispatchesTurnComplete(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Done","session_id":"s1"}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnComplete)
	}
}

func TestReadLoopContinuesOnParseError(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Write invalid JSON — readLoop should log and continue.
	if err := s.proc.WriteLine([]byte(`not valid json at all`)); err != nil {
		t.Fatalf("write bad line: %v", err)
	}

	// Write valid event after — readLoop should still be running.
	if err := s.proc.WriteLine([]byte(`{"type":"result","subtype":"success","is_error":false}`)); err != nil {
		t.Fatalf("write good line: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnComplete {
		t.Errorf("expected turn_complete after parse error recovery, got %q", evt.Kind)
	}
}

func TestReadLoopEmitsDisconnectedOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	// Close the session — should emit disconnected.
	s.Close()

	var gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !gotDisconnected {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for disconnected event")
		}
	}
}

// (TestIdleWatchdog* deleted; the per-turn idle watchdog was removed
// because it incorrectly killed the subprocess while waiting for a
// pending can_use_tool / AskUserQuestion response. See plan: t3-code
// has no equivalent watchdog and the user-facing Stop button is the
// authoritative way to abort a stuck turn.)

// TestReadLoopEmitsErrorOnOversizedLine exercises Bug B1 at the readLoop
// layer: when the subprocess writes a single line past the cap, we expect
// (1) an EventError describing the overflow, (2) the session to reach the
// disconnected terminal state, and (3) the subprocess to be reaped (no
// orphan). A regression that swallowed the error — the pre-fix behaviour —
// would leave readLoop exiting silently while the subprocess kept running.
func TestReadLoopEmitsErrorOnOversizedLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "perl -e 'print \"x\" x (33 * 1024 * 1024)'; sleep 30"
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotOverflowError, gotDisconnected bool
	timeout := time.After(15 * time.Second)
	for !(gotOverflowError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError && containsAny(evt.Content, "exceeded maximum size", "cap=") {
				gotOverflowError = true
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for oversize error + disconnected (got overflow=%v disconnected=%v)", gotOverflowError, gotDisconnected)
		}
	}

	// Process must be reaped — the orphan-process bug would leave it alive.
	select {
	case <-proc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process not reaped after oversized line (B1 regression)")
	}
}

// TestReadLoopEmitsErrorStatusOnCleanUnexpectedExit pins the
// quiet-disconnect bug fix: a subprocess that exits with status 0
// while we still expected it to be running is just as much an
// abnormal exit as one that returned a non-zero code. The previous
// gate (`if exitErr != nil`) skipped the "error" event whenever
// WaitProcessExitErr returned nil — either because the process
// exited cleanly (exit code 0) or because the 100ms wait timed out
// before the OS reaped the child. Without the "error" event, triage's
// handleSessionDied never ran, so the FE working indicator stayed
// stranded until the user manually clicked Reconnect (which then
// also failed to clean up in the round-2+ case — see
// TestCleanupThreadSynthesizesAfterRound2PlusReRound).
//
// After the fix, !s.closing is the only gate: any time the read loop
// exits without the host asking us to close, "error" fires.
func TestReadLoopEmitsErrorStatusOnCleanUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 0"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
				// Clean-exit case: meta still round-trips (Reason field
				// carries the generic "exited unexpectedly" string when
				// the exit error itself is nil). The exact reason is not
				// pinned here — it can be either the zero-error generic
				// string or a real ExitError when Wait beats the 100ms
				// timeout — but the event MUST fire.
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for error+disconnected on clean unexpected exit (gotError=%v gotDisconnected=%v)", gotError, gotDisconnected)
		}
	}
}

func TestReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 7"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
				var meta provider.ProcessExitInfo
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal exit meta: %v", err)
				}
				if meta.ExitCode != 7 {
					t.Fatalf("exitCode = %d, want 7", meta.ExitCode)
				}
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for unexpected-exit events")
		}
	}
}
