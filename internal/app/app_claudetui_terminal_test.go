package app

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/transport"
)

// newTakeControlFixture stands up one claude-tui session whose PTY runs a
// stand-in that only holds the terminal open, and registers it on a bare App so
// the ProviderTerminal* methods resolve it. The real `claude` is never spawned
// and HOME is detached, per the provider-spawn isolation rule
// (internal/kerneltest/AGENTS.md).
func newTakeControlFixture(t *testing.T) (*App, string) {
	t.Helper()
	kerneltest.DetachHome(t)

	dir := t.TempDir()
	binary := filepath.Join(dir, "mock-claude-tui")
	// Reads until the PTY closes, so the session has a live terminal to attach
	// to and Close has a process to kill. It speaks no protocol: every
	// assertion here is about the attach bookkeeping, not the wire.
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile IFS= read -r _; do :; done\n"), 0o755); err != nil {
		t.Fatalf("write PTY stand-in: %v", err)
	}

	const threadID = "thread-take-control"
	sess, err := claudetui.NewSession(context.Background(), threadID, claudetui.Config{
		Binary:  binary,
		WorkDir: dir,
		Env:     []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claudetui.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app := &App{}
	app.sessionManager().put(threadID, session{ClaudeTUI: sess})
	return app, threadID
}

// connContext is one simulated WebSocket: the ctx its RPCs carry plus the
// ConnState whose RunCleanups stands in for the socket dying.
func connContext() (context.Context, *transport.ConnState) {
	return transport.WithConnState(context.Background(), transport.ConnPrincipal{})
}

// TestSocketDeathReleasesTakeControl is the bug this file's per-connection
// bookkeeping exists for: a client that dies while holding take-control used to
// leave the lease held forever, so every Send on that thread was refused until
// the session restarted. The connection's cleanup now releases exactly that
// client's claim.
func TestSocketDeathReleasesTakeControl(t *testing.T) {
	app, threadID := newTakeControlFixture(t)
	ctx, state := connContext()

	if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
		t.Fatalf("ProviderTerminalAttach: %v", err)
	}
	if err := app.ProviderTerminalSetControl(ctx, threadID, true); err != nil {
		t.Fatalf("ProviderTerminalSetControl(true): %v", err)
	}
	sess, err := app.claudetuiSession(threadID)
	if err != nil {
		t.Fatalf("claudetuiSession: %v", err)
	}
	if !sess.HasTakeControl() {
		t.Fatal("the lease should be held after SetControl(true)")
	}
	if err := sess.Send(context.Background(), "hello", provider.SendOptions{}); err == nil ||
		!strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send under a held lease should be refused, got %v", err)
	}

	// The socket dies without a detach.
	state.RunCleanups()

	if sess.HasTakeControl() {
		t.Fatal("a dead connection must give the take-control lease back")
	}
	// Sends are accepted again: blank content trips the ordinary validation
	// rather than the lease refusal.
	if err := sess.Send(context.Background(), "   ", provider.SendOptions{}); err == nil ||
		strings.Contains(err.Error(), "take-control") {
		t.Fatalf("Send after the socket died should reach content validation, got %v", err)
	}
	if got := app.providerTerminals.get(providerTerminalKey{conn: state, threadID: threadID}); got != nil {
		t.Error("the dead connection's attachment is still recorded")
	}
}

// TestASecondConnectionDoesNotDisplaceTheFirstsAttach proves the two consequences
// of the old session-wide sink and boolean: a second client attaching used to
// replace the first client's output tee, and either client's detach used to
// strip the other's lease.
func TestASecondConnectionDoesNotDisplaceTheFirstsAttach(t *testing.T) {
	app, threadID := newTakeControlFixture(t)

	firstCtx, firstState := connContext()
	secondCtx, secondState := connContext()
	if _, err := app.ProviderTerminalAttach(firstCtx, threadID); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if err := app.ProviderTerminalSetControl(firstCtx, threadID, true); err != nil {
		t.Fatalf("first SetControl(true): %v", err)
	}
	if _, err := app.ProviderTerminalAttach(secondCtx, threadID); err != nil {
		t.Fatalf("second attach: %v", err)
	}

	// The second client cannot take the keyboard, nor type without it.
	if err := app.ProviderTerminalSetControl(secondCtx, threadID, true); err == nil ||
		!strings.Contains(err.Error(), "another client holds take-control") {
		t.Fatalf("second client acquiring a held lease should be refused, got %v", err)
	}
	input := base64.StdEncoding.EncodeToString([]byte("x"))
	if err := app.ProviderTerminalInput(secondCtx, threadID, input); err == nil ||
		!strings.Contains(err.Error(), "take-control not held") {
		t.Fatalf("second client writing input should be refused, got %v", err)
	}

	// The second client detaching leaves the first's lease and its tee alone.
	if err := app.ProviderTerminalDetach(secondCtx, threadID); err != nil {
		t.Fatalf("second detach: %v", err)
	}
	sess, err := app.claudetuiSession(threadID)
	if err != nil {
		t.Fatalf("claudetuiSession: %v", err)
	}
	if !sess.HasTakeControl() {
		t.Error("the second client's detach stripped the first client's lease")
	}
	if err := app.ProviderTerminalInput(firstCtx, threadID, input); err != nil {
		t.Errorf("the holder should still be able to type: %v", err)
	}

	// And the first client's own teardown ends the session's fan-out.
	secondState.RunCleanups()
	if err := app.ProviderTerminalDetach(firstCtx, threadID); err != nil {
		t.Fatalf("first detach: %v", err)
	}
	firstState.RunCleanups()
	if sess.HasTakeControl() {
		t.Error("no client is attached; the lease must be free")
	}
}

// TestDetachAndReleaseControlAreIdempotent proves the two paths that both
// release a claim — the client's own unmount and its socket's teardown — never
// turn the second one into an error the UI has to explain.
func TestDetachAndReleaseControlAreIdempotent(t *testing.T) {
	app, threadID := newTakeControlFixture(t)
	ctx, state := connContext()

	// Releasing control and detaching before any attach are both no-ops.
	if err := app.ProviderTerminalSetControl(ctx, threadID, false); err != nil {
		t.Fatalf("releasing control with no attachment should be a no-op, got %v", err)
	}
	if err := app.ProviderTerminalDetach(ctx, threadID); err != nil {
		t.Fatalf("detaching with no attachment should be a no-op, got %v", err)
	}
	// Taking control without attaching is refused, and says what to do.
	if err := app.ProviderTerminalSetControl(ctx, threadID, true); err == nil ||
		!strings.Contains(err.Error(), "attach") {
		t.Fatalf("taking control with no attachment should be refused, got %v", err)
	}

	if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := app.ProviderTerminalDetach(ctx, threadID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	state.RunCleanups()
	if err := app.ProviderTerminalDetach(ctx, threadID); err != nil {
		t.Fatalf("detach after the socket died should be a no-op, got %v", err)
	}
}

// TestReattachOnOneConnectionReplacesItsOwnClaim proves a pane remounting over
// one live socket does not stack claims: the displaced attachment is released,
// so the session's fan-out refcount tracks clients rather than attach calls.
func TestReattachOnOneConnectionReplacesItsOwnClaim(t *testing.T) {
	app, threadID := newTakeControlFixture(t)
	ctx, state := connContext()

	if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if err := app.ProviderTerminalSetControl(ctx, threadID, true); err != nil {
		t.Fatalf("SetControl(true) after re-attach: %v", err)
	}

	state.RunCleanups()
	sess, err := app.claudetuiSession(threadID)
	if err != nil {
		t.Fatalf("claudetuiSession: %v", err)
	}
	if sess.HasTakeControl() {
		t.Error("one connection's teardown must release the claim it re-armed")
	}
}

// TestOneConnectionArmsOneCleanup proves the cleanup is per CONNECTION, not per
// attach: a pane that remounts many times over one socket, or one socket
// holding claims on several threads, leaves exactly one closure behind, and
// that one closure releases every claim the connection still holds.
func TestOneConnectionArmsOneCleanup(t *testing.T) {
	app, threadID := newTakeControlFixture(t)
	ctx, state := connContext()

	for range 3 {
		if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
			t.Fatalf("attach: %v", err)
		}
		if err := app.ProviderTerminalDetach(ctx, threadID); err != nil {
			t.Fatalf("detach: %v", err)
		}
	}
	if _, err := app.ProviderTerminalAttach(ctx, threadID); err != nil {
		t.Fatalf("final attach: %v", err)
	}
	if err := app.ProviderTerminalSetControl(ctx, threadID, true); err != nil {
		t.Fatalf("SetControl(true): %v", err)
	}
	app.providerTerminals.mu.Lock()
	armed := len(app.providerTerminals.armed)
	app.providerTerminals.mu.Unlock()
	if armed != 1 {
		t.Fatalf("one connection should be armed once, got %d", armed)
	}

	state.RunCleanups()
	sess, err := app.claudetuiSession(threadID)
	if err != nil {
		t.Fatalf("claudetuiSession: %v", err)
	}
	if sess.HasTakeControl() {
		t.Error("the connection's one cleanup must release the claim it held")
	}
	app.providerTerminals.mu.Lock()
	armed, claims := len(app.providerTerminals.armed), len(app.providerTerminals.byCaller)
	app.providerTerminals.mu.Unlock()
	if armed != 0 || claims != 0 {
		t.Errorf("a dead connection leaves no bookkeeping behind: armed=%d claims=%d", armed, claims)
	}
}
