package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
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
	proc, err := provider.Spawn(ctx, providertest.Lifetime(), provider.SpawnConfig{Binary: "cat"})
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
	proc, err := provider.Spawn(ctx, providertest.Lifetime(), provider.SpawnConfig{
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

// TestReadLoopEmitsErrorStatusOnUnexpectedExit pins that any read-loop
// exit the host did not ask for (!s.closing is the only gate) emits the
// "error" status, exit code 0 included. The previous gate
// (`if exitErr != nil`) skipped it whenever WaitProcessExitErr returned
// nil — a clean exit, or the 100ms wait timing out before the OS reaped
// the child. Without the "error" event, triage's handleSessionDied never
// ran, so the FE working indicator stayed stranded until the user clicked
// Reconnect. The triage side (handleSessionDied's truncated complete for
// an open turn) is pinned by TestSessionStatusErrorSynthesizesAfterSelfWake.
func TestReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			proc, err := provider.Spawn(ctx, providertest.Lifetime(), provider.SpawnConfig{
				Binary: "sh",
				Args:   []string{"-c", fmt.Sprintf("sleep 0.05; exit %d", code)},
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
			for !gotError || !gotDisconnected {
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
						if meta.ExitCode != code {
							t.Fatalf("exitCode = %d, want %d", meta.ExitCode, code)
						}
					case "disconnected":
						gotDisconnected = true
					}
				case <-timeout:
					t.Fatalf("timeout waiting for error+disconnected on unexpected exit (gotError=%v gotDisconnected=%v)", gotError, gotDisconnected)
				}
			}
		})
	}
}
