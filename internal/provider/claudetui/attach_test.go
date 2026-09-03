package claudetui

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// noSink is the stand-in tee for attachments made without a live PTY: the
// refcount and lease rules are what these tests are about, and none of them
// produce output.
func noSink(string, uint64, []byte) {}

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

	attachment := s.addAttachment(noSink)
	if err := attachment.SetControl(true); err != nil {
		t.Fatalf("SetControl(true) on a free lease: %v", err)
	}
	if !s.HasTakeControl() {
		t.Fatal("HasTakeControl should report true once an attachment holds the lease")
	}
	err := s.Send(ctx, "hello", provider.SendOptions{})
	if err == nil || !strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send while a human holds control should be refused with a take-control error, got %v", err)
	}

	// Releasing the lease must let Send proceed past the lease gate. Blank
	// content then trips the normal validation — NOT the lease error — proving
	// the gate only fires while held.
	if err := attachment.SetControl(false); err != nil {
		t.Fatalf("SetControl(false) by the holder: %v", err)
	}
	if s.HasTakeControl() {
		t.Fatal("HasTakeControl should report false after the holder releases")
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
	attachment := s.addAttachment(noSink)

	if err := attachment.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "take-control not held") {
		t.Fatalf("WriteInput without the lease should be refused, got %v", err)
	}

	if err := attachment.SetControl(true); err != nil {
		t.Fatalf("SetControl(true): %v", err)
	}
	// Lease held: WriteInput clears the gate and reaches writePTY, which reports
	// the missing terminal rather than the lease error.
	if err := attachment.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "no live terminal") {
		t.Fatalf("WriteInput with the lease held should pass the gate and reach the PTY write, got %v", err)
	}
}

// TestASecondViewerCannotTakeTheKeyboard proves the lease is single-holder:
// while A holds it, B's acquire is refused, B cannot write input, and B
// releasing does not strip A's lease.
func TestASecondViewerCannotTakeTheKeyboard(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	viewerA := s.addAttachment(noSink)
	viewerB := s.addAttachment(noSink)

	if err := viewerA.SetControl(true); err != nil {
		t.Fatalf("A acquiring a free lease: %v", err)
	}
	if err := viewerB.SetControl(true); err == nil || !strings.Contains(err.Error(), "another client holds take-control") {
		t.Fatalf("B acquiring a held lease should be refused, got %v", err)
	}
	if viewerB.HoldsControl() {
		t.Error("a refused acquire must not leave B holding the lease")
	}
	if err := viewerB.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "take-control not held") {
		t.Fatalf("B writing input without the lease should be refused, got %v", err)
	}
	// B releasing a lease it never held is a no-op, not a theft.
	if err := viewerB.SetControl(false); err != nil {
		t.Fatalf("B releasing a lease it does not hold should be a no-op, got %v", err)
	}
	if !viewerA.HoldsControl() {
		t.Error("B's release stripped A's lease")
	}

	// A releasing frees it for B.
	if err := viewerA.SetControl(false); err != nil {
		t.Fatalf("A releasing its own lease: %v", err)
	}
	if err := viewerB.SetControl(true); err != nil {
		t.Fatalf("B acquiring a freed lease: %v", err)
	}
	if !s.HasTakeControl() {
		t.Error("the session should report the lease held by B")
	}
}

// TestReleaseKeepsTheOtherViewerAttached proves one client detaching leaves the
// other's output tee armed — the fan-out is refcounted, not last-attach-wins —
// and that the tee is torn down only when the last claim goes.
func TestReleaseKeepsTheOtherViewerAttached(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}

	var seen []string
	viewerA := s.addAttachment(func(_ string, _ uint64, data []byte) { seen = append(seen, string(data)) })
	viewerB := s.addAttachment(noSink)

	s.onPTYOutput(testThread, "term-1", 1, []byte("first"))
	viewerA.Release()
	s.onPTYOutput(testThread, "term-1", 2, []byte("second"))

	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("output seen = %q, want both chunks: one viewer detaching must not blind the other", seen)
	}

	viewerB.Release()
	s.mu.Lock()
	sink, attached := s.sink, len(s.attachments)
	s.mu.Unlock()
	if sink != nil || attached != 0 {
		t.Fatalf("after the last release: sink!=nil=%v, attachments=%d, want the tee torn down", sink != nil, attached)
	}
	s.onPTYOutput(testThread, "term-1", 3, []byte("ignored"))
	if len(seen) != 2 {
		t.Fatalf("output after the last release reached a sink: %q", seen)
	}
}

// TestReleaseGivesBackTheLease proves the teardown path releases control on the
// holder's behalf — which is what a dead WebSocket runs — and that releasing an
// attachment that never held it is a no-op rather than a lease strip.
func TestReleaseGivesBackTheLease(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	holder := s.addAttachment(noSink)
	watcher := s.addAttachment(noSink)
	if err := holder.SetControl(true); err != nil {
		t.Fatalf("SetControl(true): %v", err)
	}

	// A teardown of the attachment that never held it changes nothing.
	watcher.Release()
	if !s.HasTakeControl() {
		t.Fatal("releasing a watcher stripped the holder's lease")
	}

	holder.Release()
	if s.HasTakeControl() {
		t.Fatal("the holder's teardown must give the lease back")
	}
	if err := s.Send(context.Background(), "   ", provider.SendOptions{}); err == nil || strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send after the holder's teardown should reach content validation, got %v", err)
	}

	// Idempotent: a second release (the client's own detach arriving after the
	// socket cleanup, or the other way round) is a no-op.
	holder.Release()
	if err := holder.SetControl(false); err != nil {
		t.Fatalf("releasing control on a released attachment should be a no-op, got %v", err)
	}
	if err := holder.SetControl(true); err == nil || !strings.Contains(err.Error(), "no longer attached") {
		t.Fatalf("acquiring control on a released attachment should be refused, got %v", err)
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
	s.addAttachment(func(terminalID string, seq uint64, data []byte) {
		gotTerminal, gotSeq, gotData = terminalID, seq, string(data)
	})
	s.onPTYOutput(testThread, "term-9", 7, []byte("hi"))
	if gotTerminal != "term-9" || gotSeq != 7 || gotData != "hi" {
		t.Fatalf("sink received (%q, %d, %q), want (term-9, 7, hi)", gotTerminal, gotSeq, gotData)
	}

	// Detached: no sink wired. Must not panic.
	s.mu.Lock()
	s.sink = nil
	s.mu.Unlock()
	s.onPTYOutput(testThread, "term-9", 8, []byte("ignored"))
}

// TestAttachAndIORequireLiveTerminal proves every take-control PTY operation
// fails loudly when there's no live terminal instead of dereferencing a nil
// manager, and that a refused attach leaves no claim behind.
func TestAttachAndIORequireLiveTerminal(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}

	if _, _, err := s.AttachTerminal(noSink); err == nil {
		t.Error("AttachTerminal with no live terminal should error")
	}
	s.mu.Lock()
	attached, sink := len(s.attachments), s.sink
	s.mu.Unlock()
	if attached != 0 || sink != nil {
		t.Errorf("a refused attach armed state: attachments=%d, sink!=nil=%v", attached, sink != nil)
	}
	if _, _, err := s.AttachTerminal(nil); err == nil {
		t.Error("AttachTerminal with no sink should error")
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

// TestNilAttachmentIsSafe proves the App-side "this caller has no claim" answer
// is a plain refusal on every method rather than a panic: ProviderTerminalInput
// and the detach path both reach these with nil.
func TestNilAttachmentIsSafe(t *testing.T) {
	var none *TerminalAttachment
	none.Release()
	if none.HoldsControl() {
		t.Error("a nil attachment must not report holding the lease")
	}
	if err := none.WriteInput([]byte("x")); err == nil || !strings.Contains(err.Error(), "take-control not held") {
		t.Fatalf("WriteInput on a nil attachment should be refused, got %v", err)
	}
	if err := none.SetControl(true); err == nil {
		t.Error("SetControl on a nil attachment should be refused")
	}
}
