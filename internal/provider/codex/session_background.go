package codex

import (
	"context"
	"fmt"
)

// CleanBackgroundTerminals asks the Codex app-server to terminate every
// running unified-exec background PTY for this session's thread. The call
// is thread-wide by design: the app-server protocol exposes no per-process
// kill RPC for model-initiated background terminals. See
// docs/references/codex.md#known-upstream-constraints.
//
// Wire contract is owned by the Codex source of truth:
// /Users/randy/repos/codex-source/codex-rs/app-server-protocol/src/protocol/v2.rs
// (ThreadBackgroundTerminalsCleanParams / ThreadBackgroundTerminalsCleanResponse).
// The response body is empty on success — the observable effect is a stream
// of `item/completed` events for each terminated PTY that flow through our
// existing triage path (Phase 2's sibling synthesis fires per completion).
//
// Safe to call from any goroutine: sendRequest handles correlation under
// the session's internal locks. Returns ctx.Err() on cancellation, or a
// wrapped error on RPC failure.
func (s *Session) CleanBackgroundTerminals(ctx context.Context) error {
	// Tests install a cleanBackgroundTerminalsFn override so the binding
	// layer can verify its session-lookup / provider-mismatch plumbing
	// without spinning up a real app-server. Production NewSession never
	// sets it; the wire path below is the only branch that runs in
	// production.
	if s.cleanBackgroundTerminalsFn != nil {
		return s.cleanBackgroundTerminalsFn(ctx)
	}
	// NewSession validates codexThreadID during the start/resume handshake
	// and Close's the session on failure, so a live Session always has this
	// populated. The explicit guard mirrors Probe/Resume so a future caller
	// that constructs a partial Session for testing still gets a specific
	// error rather than the app-server rejecting an empty threadId with a
	// less actionable message.
	if s.codexThreadID == "" {
		return fmt.Errorf("codex: thread/backgroundTerminals/clean: session has no thread id")
	}
	if _, err := s.sendRequest(ctx, "thread/backgroundTerminals/clean", map[string]any{
		"threadId": s.codexThreadID,
	}); err != nil {
		return fmt.Errorf("codex: thread/backgroundTerminals/clean: %w", err)
	}
	return nil
}
