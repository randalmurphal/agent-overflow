package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/provider/codex"
)

// TestCleanCodexBackgroundTerminals_SessionMissing covers the first
// input-validation branch: a caller that reaches the binding before
// Start / after Close must see a clear error, not a silent no-op. The
// Stop-all button in the tray fires this per-thread; a silent drop
// would leave the UI feeling broken.
func TestCleanCodexBackgroundTerminals_SessionMissing(t *testing.T) {
	a := NewApp()

	err := a.CleanCodexBackgroundTerminals("no-such-thread")
	if err == nil {
		t.Fatal("CleanCodexBackgroundTerminals with no session: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error should mention missing session, got: %v", err)
	}
}

// TestCleanCodexBackgroundTerminals_ProviderMismatch covers the Claude
// branch: the clean RPC is Codex-only. A caller that routed a Claude
// thread to this binding is programming against the wrong API and must
// see a loud error — Claude has its own stop primitive (StopClaudeTask).
func TestCleanCodexBackgroundTerminals_ProviderMismatch(t *testing.T) {
	a := NewApp()

	// Install a session entry whose provider-level typed fields are both
	// nil — mirrors a Claude session for the narrow contract this test
	// probes (the binding only cares that sess.codex is nil).
	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude"}
	a.mu.Unlock()

	err := a.CleanCodexBackgroundTerminals("claude-thread")
	if err == nil {
		t.Fatal("CleanCodexBackgroundTerminals on non-Codex thread: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is not a Codex thread") {
		t.Errorf("error should mention provider mismatch, got: %v", err)
	}
}

// TestCleanCodexBackgroundTerminals_ShuttingDown short-circuits the
// binding when the App is mid-teardown. This mirrors every other
// binding entry point (RespondToApproval, SendMessage, StopClaudeTask,
// ...) that fails fast with ErrShuttingDown rather than racing against
// a subsystem that's being torn down.
func TestCleanCodexBackgroundTerminals_ShuttingDown(t *testing.T) {
	a := NewApp()
	a.shuttingDown.Store(true)

	err := a.CleanCodexBackgroundTerminals("any-thread")
	if err != ErrShuttingDown {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
}

// TestCleanCodexBackgroundTerminals_RoundTripSucceeds drives the happy
// path: session exists, provider is Codex, RPC returns nil. Uses the
// test-only NewCleanBackgroundTerminalsTestSession helper so we don't
// need to spin up a real app-server subprocess — the session-level
// tests (TestSession_CleanBackgroundTerminals_SuccessRoundTrip etc.)
// already pin the wire shape; this test proves the binding's session
// lookup + context plumbing hook through correctly.
func TestCleanCodexBackgroundTerminals_RoundTripSucceeds(t *testing.T) {
	a := NewApp()

	var called atomic.Bool
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(ctx context.Context) error {
		if ctx == nil {
			t.Error("binding passed nil context to session")
		}
		// The binding must install a deadline so a wedged provider
		// can't hang the Wails call indefinitely.
		if _, ok := ctx.Deadline(); !ok {
			t.Error("binding must pass a context with a deadline")
		}
		called.Store(true)
		return nil
	})

	a.mu.Lock()
	a.sessions["codex-thread"] = session{provider: "codex", codex: fakeSess}
	a.mu.Unlock()

	if err := a.CleanCodexBackgroundTerminals("codex-thread"); err != nil {
		t.Fatalf("CleanCodexBackgroundTerminals: %v", err)
	}
	if !called.Load() {
		t.Fatal("binding did not reach the session's CleanBackgroundTerminals")
	}
}

// TestCleanCodexBackgroundTerminals_SurfacesProviderError confirms the
// binding propagates the session-level error verbatim. "User-facing
// state" per Core Principle 5 — the tray Stop-all button will render
// whatever message the app-server sent, so dropping or wrapping it
// generically would be a regression in diagnostic quality.
func TestCleanCodexBackgroundTerminals_SurfacesProviderError(t *testing.T) {
	a := NewApp()

	want := errors.New("codex: thread/backgroundTerminals/clean: thread not found")
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(_ context.Context) error {
		return want
	})

	a.mu.Lock()
	a.sessions["codex-thread"] = session{provider: "codex", codex: fakeSess}
	a.mu.Unlock()

	err := a.CleanCodexBackgroundTerminals("codex-thread")
	if err == nil {
		t.Fatal("expected propagated error, got nil")
	}
	if !errors.Is(err, want) && !strings.Contains(err.Error(), "thread not found") {
		t.Errorf("expected provider error surfaced verbatim, got: %v", err)
	}
}
