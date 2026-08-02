package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestSession_CleanBackgroundTerminals_SuccessRoundTrip drives the happy
// path end to end against a fake app-server: initialize + thread/start
// complete, then CleanBackgroundTerminals writes a
// thread/backgroundTerminals/clean request carrying the correct
// threadId, the fake answers with an empty success body, and the method
// returns nil. The fake's match arm is keyed on the exact method string
// — a typo on our side would drop into the fake's silent default branch
// and the call would hit the 30s request timeout instead of succeeding,
// so this also covers the wire method-name assertion.
func TestSession_CleanBackgroundTerminals_SuccessRoundTrip(t *testing.T) {
	capturePath := t.TempDir() + "/clean-request.json"
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-bg\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/backgroundTerminals/clean"'; then
        # Capture the request frame to disk so the test can assert
        # the exact wire shape — method + threadId.
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
    fi
done
`, capturePath)

	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if err := s.CleanBackgroundTerminals(context.Background()); err != nil {
		t.Fatalf("CleanBackgroundTerminals: %v", err)
	}

	// Assert the captured request frame. We deserialize so we don't
	// couple to JSON key ordering — only the values matter.
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ThreadID string `json:"threadId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode captured request: %v (raw: %s)", err, string(raw))
	}
	if frame.JSONRPC != "2.0" {
		t.Errorf("frame.jsonrpc = %q, want 2.0", frame.JSONRPC)
	}
	if frame.Method != "thread/backgroundTerminals/clean" {
		t.Errorf("frame.method = %q, want thread/backgroundTerminals/clean", frame.Method)
	}
	if frame.Params.ThreadID != "codex-thread-bg" {
		t.Errorf("frame.params.threadId = %q, want codex-thread-bg", frame.Params.ThreadID)
	}
}

// TestSession_CleanBackgroundTerminals_ErrorResponse confirms that a
// JSON-RPC error reply surfaces as a non-nil error whose message
// contains the server-supplied detail so the caller can render it to
// the user. Injects the response directly into the pending channel
// rather than going through a fake process — keeps the test in-process
// and avoids any flakes from subprocess ordering.
func TestSession_CleanBackgroundTerminals_ErrorResponse(t *testing.T) {
	procCtx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(procCtx, provider.SpawnConfig{
		Binary: "sh",
		// No trailing sleep so Close() exits cleanly once stdin closes —
		// keeps the test under a second rather than parking in
		// shutdownGrace.
		Args: []string{"-c", "cat > /dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID("codex-thread-bg-err")
	go s.readLoop()

	cleanDone := make(chan error, 1)
	go func() {
		cleanDone <- s.CleanBackgroundTerminals(context.Background())
	}()

	ch, rpcID := waitForPending(t, s, 3*time.Second)
	ch <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32603,"message":"thread not found"}}`, rpcID))

	select {
	case err := <-cleanDone:
		if err == nil {
			t.Fatal("CleanBackgroundTerminals: expected error, got nil")
		}
		if !strings.Contains(err.Error(), "thread not found") {
			t.Errorf("error missing server-supplied detail: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CleanBackgroundTerminals never returned")
	}
}

// TestSession_CleanBackgroundTerminals_ContextCanceled confirms that a
// caller whose context is canceled before (or during) the wait for a
// response gets ctx.Err() back promptly — not a timeout, not a hang.
// The pre-cancel variant exercises the fast path; elapsed time must be
// far below the 30s request timeout.
func TestSession_CleanBackgroundTerminals_ContextCanceled(t *testing.T) {
	procCtx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(procCtx, provider.SpawnConfig{
		Binary: "sh",
		// No trailing sleep — as soon as stdin closes the shell exits,
		// so proc.Close() doesn't stall in shutdownGrace. We only need
		// the subprocess alive during the single WriteLine call below.
		Args: []string{"-c", "cat > /dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID("codex-thread-bg-cancel")
	go s.readLoop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call so sendRequest sees ctx.Done immediately

	start := time.Now()
	err = s.CleanBackgroundTerminals(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("CleanBackgroundTerminals: expected context-canceled error, got nil")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
	// A hang would sit on the 30s request timeout; anything under a
	// couple seconds proves we short-circuited on ctx.Done.
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, expected near-immediate return on pre-canceled ctx", elapsed)
	}
}

// TestSession_CleanBackgroundTerminals_MidWaitCancel pins the
// mid-wait cancel path: a caller whose context is canceled AFTER the
// request frame is on the wire but BEFORE the response arrives must
// still unblock promptly with ctx.Err(). Distinct from the pre-cancel
// case — that takes the fast path inside sendRequest before the
// pending-channel register; this one exercises the <-ctx.Done() arm
// of the select that parks on the response. A regression that, say,
// dropped the ctx.Done branch would sit on the full 30s request
// timeout instead.
func TestSession_CleanBackgroundTerminals_MidWaitCancel(t *testing.T) {
	procCtx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(procCtx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID("codex-thread-bg-midcancel")
	go s.readLoop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cleanDone := make(chan error, 1)
	go func() {
		cleanDone <- s.CleanBackgroundTerminals(ctx)
	}()

	// Wait until sendRequest has parked on a pending-channel — proves
	// the frame hit the wire and we're mid-wait, not mid-WriteLine.
	_, _ = waitForPending(t, s, 3*time.Second)

	// Now cancel. The pending select must observe ctx.Done and
	// unblock; the 30s deadline never fires.
	start := time.Now()
	cancel()
	select {
	case err := <-cleanDone:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("CleanBackgroundTerminals: expected ctx.Canceled error after mid-wait cancel, got nil")
		}
		if !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Errorf("error should wrap context.Canceled, got: %v", err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("mid-wait cancel took %v, want <2s (pending-select ctx.Done arm may be missing)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CleanBackgroundTerminals never returned after mid-wait ctx cancel (regression: ctx.Done arm missing)")
	}
}

// waitForPending blocks until exactly one pending request is registered
// on s and returns its channel + id. Used by tests that drive the
// pending channel directly instead of through a subprocess echo.
func waitForPending(t *testing.T, s *Session, within time.Duration) (chan json.RawMessage, int64) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for pending request registration")
			return nil, 0
		default:
		}
		s.mu.Lock()
		for id, c := range s.pending {
			s.mu.Unlock()
			return c, id
		}
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}
