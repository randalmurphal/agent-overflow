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

// --- per-row stop: TerminateCodexBackgroundTerminal ---

// newTerminateBindingApp installs a Codex session whose per-row stop RPC
// resolves from `fn`, so each test below drives only the binding's own
// behaviour. The wire shape itself is pinned in the codex package
// (session_background_terminals_test.go).
func newTerminateBindingApp(fn func(ctx context.Context, processID string) (bool, error)) *App {
	a := NewApp()
	a.mu.Lock()
	a.sessions["codex-thread"] = session{
		provider: "codex",
		codex:    codex.NewTerminateBackgroundTerminalTestSession(fn),
	}
	a.mu.Unlock()
	return a
}

// TestTerminateCodexBackgroundTerminal_SessionMissing mirrors the
// Stop-all case: a per-row Stop pressed after the session closed must
// report why rather than look like a no-op.
func TestTerminateCodexBackgroundTerminal_SessionMissing(t *testing.T) {
	a := NewApp()

	terminated, err := a.TerminateCodexBackgroundTerminal("no-such-thread", "42")
	if err == nil {
		t.Fatal("TerminateCodexBackgroundTerminal with no session: expected error, got nil")
	}
	if terminated {
		t.Error("terminated must be false on the error path")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error should mention missing session, got: %v", err)
	}
}

// TestTerminateCodexBackgroundTerminal_ProviderMismatch pins the branch
// that keeps the two per-row stop primitives apart. Claude rows carry a
// task id and stop through StopClaudeTask; routing one here would send a
// task id into a process-id parameter, so the binding must refuse loudly
// rather than forward a value the app-server would reject as invalid.
func TestTerminateCodexBackgroundTerminal_ProviderMismatch(t *testing.T) {
	a := NewApp()

	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude"}
	a.mu.Unlock()

	if _, err := a.TerminateCodexBackgroundTerminal("claude-thread", "42"); err == nil {
		t.Fatal("TerminateCodexBackgroundTerminal on non-Codex thread: expected error, got nil")
	} else if !strings.Contains(err.Error(), "is not a Codex thread") {
		t.Errorf("error should mention provider mismatch, got: %v", err)
	}
}

// TestTerminateCodexBackgroundTerminal_ShuttingDown mirrors every other
// binding entry point: fail fast instead of racing a subsystem that is
// being torn down.
func TestTerminateCodexBackgroundTerminal_ShuttingDown(t *testing.T) {
	a := NewApp()
	a.shuttingDown.Store(true)

	terminated, err := a.TerminateCodexBackgroundTerminal("any-thread", "42")
	if err != ErrShuttingDown {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
	if terminated {
		t.Error("terminated must be false when the app is shutting down")
	}
}

// TestTerminateCodexBackgroundTerminal_ForwardsProcessID is the core
// contract: the process id the tray row carries reaches the session
// verbatim, under a deadline, and the wire's `terminated` answer comes
// back unchanged. Forwarding the wrong id would kill a DIFFERENT running
// shell, so this asserts the exact value rather than just "non-empty".
func TestTerminateCodexBackgroundTerminal_ForwardsProcessID(t *testing.T) {
	var gotProcessID string
	var sawDeadline bool
	a := newTerminateBindingApp(func(ctx context.Context, processID string) (bool, error) {
		if ctx == nil {
			t.Error("binding passed nil context to session")
		} else if _, ok := ctx.Deadline(); ok {
			sawDeadline = true
		}
		gotProcessID = processID
		return true, nil
	})

	terminated, err := a.TerminateCodexBackgroundTerminal("codex-thread", "1734029")
	if err != nil {
		t.Fatalf("TerminateCodexBackgroundTerminal: %v", err)
	}
	if !terminated {
		t.Error("terminated = false, want the session's true passed through")
	}
	if gotProcessID != "1734029" {
		t.Errorf("session received process id %q, want 1734029", gotProcessID)
	}
	if !sawDeadline {
		t.Error("binding must pass a context with a deadline")
	}
}

// TestTerminateCodexBackgroundTerminal_MatchedNothingIsNotAnError pins
// the wire's `terminated:false` as a STATE answer. Upstream returns it
// when the process store holds no entry for the id — already exited, or
// another thread's — and the RPC itself succeeded
// (codex-rs/core/src/unified_exec/process_manager.rs terminate_process).
// Collapsing it into an error would make an ordinary race look like a
// failure; dropping it entirely would leave the UI with nothing to say.
func TestTerminateCodexBackgroundTerminal_MatchedNothingIsNotAnError(t *testing.T) {
	a := newTerminateBindingApp(func(context.Context, string) (bool, error) {
		return false, nil
	})

	terminated, err := a.TerminateCodexBackgroundTerminal("codex-thread", "42")
	if err != nil {
		t.Fatalf("matched-nothing must not be an error, got: %v", err)
	}
	if terminated {
		t.Error("terminated = true, want the session's false passed through")
	}
}

// TestTerminateCodexBackgroundTerminal_SurfacesProviderError confirms the
// app-server's message reaches the caller intact — the tray renders it in
// a toast, so a generic wrap would be a diagnostic regression
// (Core Principle 5).
func TestTerminateCodexBackgroundTerminal_SurfacesProviderError(t *testing.T) {
	want := errors.New("codex: thread/backgroundTerminals/terminate 42: invalid background terminal process id")
	a := newTerminateBindingApp(func(context.Context, string) (bool, error) {
		return false, want
	})

	terminated, err := a.TerminateCodexBackgroundTerminal("codex-thread", "42")
	if err == nil {
		t.Fatal("expected propagated error, got nil")
	}
	if terminated {
		t.Error("terminated must be false on the error path")
	}
	if !errors.Is(err, want) && !strings.Contains(err.Error(), "invalid background terminal process id") {
		t.Errorf("expected provider error surfaced verbatim, got: %v", err)
	}
}

// TestTerminateCodexBackgroundTerminal_RefusesBlankProcessID proves the
// argument guard is structural rather than caller discipline: the
// session-level validation runs even for a test session that overrides
// the wire call, so no path can send an empty processId (which upstream
// rejects with -32600) and no override can quietly accept one.
func TestTerminateCodexBackgroundTerminal_RefusesBlankProcessID(t *testing.T) {
	reached := false
	a := newTerminateBindingApp(func(context.Context, string) (bool, error) {
		reached = true
		return true, nil
	})

	for _, blank := range []string{"", "   "} {
		terminated, err := a.TerminateCodexBackgroundTerminal("codex-thread", blank)
		if err == nil {
			t.Fatalf("blank process id %q: expected error, got nil", blank)
		}
		if terminated {
			t.Errorf("blank process id %q: terminated must be false", blank)
		}
		if !strings.Contains(err.Error(), "process id required") {
			t.Errorf("blank process id %q: unexpected error: %v", blank, err)
		}
	}
	if reached {
		t.Error("a blank process id must never reach the terminate RPC")
	}
}
