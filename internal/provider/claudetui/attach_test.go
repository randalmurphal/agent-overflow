package claudetui

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestTakeControlLeaseGatesSend proves the lease blocks AO's programmatic Send
// while a human holds the terminal, and that releasing the lease lets Send fall
// through to its normal validation (so the lease check can't be confused with
// the other reject-before-write branches).
func TestTakeControlLeaseGatesSend(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	ctx := context.Background()

	if s.HasTakeControl() {
		t.Fatal("a fresh session must not start with take-control held")
	}

	s.SetTakeControl(true)
	if !s.HasTakeControl() {
		t.Fatal("HasTakeControl should report true after SetTakeControl(true)")
	}
	err := s.Send(ctx, "hello", provider.SendOptions{})
	if err == nil || !strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send while a human holds control should be refused with a take-control error, got %v", err)
	}

	// Releasing the lease must let Send proceed past the lease gate. Blank
	// content then trips the normal validation — NOT the lease error — proving
	// the gate only fires while held.
	s.SetTakeControl(false)
	if s.HasTakeControl() {
		t.Fatal("HasTakeControl should report false after SetTakeControl(false)")
	}
	err = s.Send(ctx, "   ", provider.SendOptions{})
	if err == nil || strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send with the lease released should reach content validation, not the lease error, got %v", err)
	}
	if !strings.Contains(err.Error(), "text or image content") {
		t.Fatalf("blank Send should fail content validation, got %v", err)
	}
}

// TestTakeControlLeaseGatesWriteInput proves WriteInput is refused without the
// lease (read-only attach can't inject input) and, once held, passes the lease
// gate to reach the PTY write — surfaced here as the no-terminal error because
// the bare session has no live PTY.
func TestTakeControlLeaseGatesWriteInput(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}

	if err := s.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "take-control not held") {
		t.Fatalf("WriteInput without the lease should be refused, got %v", err)
	}

	s.SetTakeControl(true)
	// Lease held: WriteInput clears the gate and reaches writePTY, which reports
	// the missing terminal rather than the lease error.
	if err := s.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "no live terminal") {
		t.Fatalf("WriteInput with the lease held should pass the gate and reach the PTY write, got %v", err)
	}
}

// TestDetachReleasesControlAndSink proves DetachTerminal both stops output
// fan-out and drops the take-control lease, so a closed pane leaves no live
// attach behind.
func TestDetachReleasesControlAndSink(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	s.SetTakeControl(true)
	s.sink = func(string, uint64, []byte) {}

	s.DetachTerminal()

	if s.HasTakeControl() {
		t.Error("DetachTerminal should release the take-control lease")
	}
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		t.Error("DetachTerminal should clear the output sink")
	}
}

// TestOnPTYOutputFansToSink proves PTY output is forwarded to the attached sink
// with the terminal id intact, and that a detached session (nil sink) is a safe
// no-op rather than a panic.
func TestOnPTYOutputFansToSink(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}

	var (
		gotTerminal string
		gotSeq      uint64
		gotData     string
	)
	s.sink = func(terminalID string, seq uint64, data []byte) {
		gotTerminal, gotSeq, gotData = terminalID, seq, string(data)
	}
	s.onPTYOutput(testThread, "term-9", 7, []byte("hi"))
	if gotTerminal != "term-9" || gotSeq != 7 || gotData != "hi" {
		t.Fatalf("sink received (%q, %d, %q), want (term-9, 7, hi)", gotTerminal, gotSeq, gotData)
	}

	// Detached: no sink wired. Must not panic.
	s.sink = nil
	s.onPTYOutput(testThread, "term-9", 8, []byte("ignored"))
}

// TestAttachAndIORequireLiveTerminal proves every take-control PTY operation
// fails loudly when there's no live terminal instead of dereferencing a nil
// manager.
func TestAttachAndIORequireLiveTerminal(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}

	if _, err := s.AttachTerminal(func(string, uint64, []byte) {}); err == nil {
		t.Error("AttachTerminal with no live terminal should error")
	}
	if _, err := s.TerminalReplaySnapshot(); err == nil {
		t.Error("TerminalReplaySnapshot with no live terminal should error")
	}
	if err := s.ResizeTerminal(24, 80); err == nil {
		t.Error("ResizeTerminal with no live terminal should error")
	}
	if err := s.RefreshTerminal(); err == nil {
		t.Error("RefreshTerminal with no live terminal should error")
	}
	if id := s.TerminalID(); id != "" {
		t.Errorf("TerminalID on a session with no terminal = %q, want empty", id)
	}
}
