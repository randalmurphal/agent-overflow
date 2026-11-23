package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// An agent's terminal lives in the agent's own Codex thread, whose process
// table is separate from the root's, so a stop of it must name that thread.
// The owner launch resolves through the session's child ownership.
func TestBackgroundTerminalRPCsTargetTheOwnerThread(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	s.collab.childParentByThread = map[string]string{"codex-child-thread": "spawn-1"}

	done := make(chan error, 2)
	go func() {
		killed, err := s.TerminateBackgroundTerminal(context.Background(), "spawn-1", "42")
		if err == nil && !killed {
			err = errors.New("terminated = false, want the wire's true")
		}
		done <- err
	}()
	answerer := newPendingAnswerer(s)
	answerer.answer(t, `{"terminated":true}`)
	if err := <-done; err != nil {
		t.Fatalf("TerminateBackgroundTerminal: %v", err)
	}
	go func() { done <- s.CleanBackgroundTerminals(context.Background(), "spawn-1") }()
	answerer.answer(t, `{}`)
	if err := <-done; err != nil {
		t.Fatalf("CleanBackgroundTerminals: %v", err)
	}

	frames := waitForCapturedFrames(t, capturePath, 2, backgroundTerminalTestTimeout)
	for i, want := range []string{"thread/backgroundTerminals/terminate", "thread/backgroundTerminals/clean"} {
		if frames[i].Method != want || frames[i].Params.ThreadID != "codex-child-thread" {
			t.Errorf("frame %d = %s on %q, want %s on the agent's thread codex-child-thread", i, frames[i].Method, frames[i].Params.ThreadID, want)
		}
	}
}

// A launch this session owns no thread for, and an agent thread the
// app-server no longer has loaded, are the same answer: the agent is gone
// and nothing of it is left to stop. The root thread's refusal is not.
func TestBackgroundTerminalRPCsReportAGoneOwner(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	s.collab.childParentByThread = map[string]string{"codex-child-thread": "spawn-1"}

	if _, err := s.TerminateBackgroundTerminal(context.Background(), "spawn-unowned", "42"); !errors.Is(err, ErrBackgroundTerminalOwnerGone) {
		t.Fatalf("terminate for an unowned launch: err = %v, want ErrBackgroundTerminalOwnerGone", err)
	}
	if err := s.CleanBackgroundTerminals(context.Background(), "spawn-unowned"); !errors.Is(err, ErrBackgroundTerminalOwnerGone) {
		t.Fatalf("clean for an unowned launch: err = %v, want ErrBackgroundTerminalOwnerGone", err)
	}

	refuse := func(threadID string) {
		t.Helper()
		ch, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
		ch <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"thread not found: %s"}}`, rpcID, threadID))
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.TerminateBackgroundTerminal(context.Background(), "spawn-1", "42")
		done <- err
	}()
	refuse("codex-child-thread")
	if err := <-done; !errors.Is(err, ErrBackgroundTerminalOwnerGone) {
		t.Fatalf("terminate in an unloaded agent thread: err = %v, want ErrBackgroundTerminalOwnerGone", err)
	}
	go func() { done <- s.CleanBackgroundTerminals(context.Background(), "") }()
	refuse("codex-root-thread")
	if err := <-done; err == nil || errors.Is(err, ErrBackgroundTerminalOwnerGone) {
		t.Fatalf("clean refused for the root thread: err = %v, want a plain error", err)
	}
	waitForCapturedFrames(t, capturePath, 2, backgroundTerminalTestTimeout)
}
