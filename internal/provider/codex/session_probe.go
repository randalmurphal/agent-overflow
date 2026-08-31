package codex

import (
	"context"
	"encoding/json"
	"fmt"
)

// ThreadStatusKind enumerates the closed set of `thread.status.type`
// values the Codex app-server reports on `thread/read`. The app relies
// on this enum to pick a reconciliation strategy after a restart — see
// `codexthread.Service.ReconcileOnReopen` for the consumer.
//
// Source of truth:
//
//	codex-rs/app-server-protocol/schema/typescript/v2/ThreadStatus.ts
type ThreadStatusKind string

const (
	ThreadStatusIdle        ThreadStatusKind = "idle"
	ThreadStatusActive      ThreadStatusKind = "active"
	ThreadStatusNotLoaded   ThreadStatusKind = "notLoaded"
	ThreadStatusSystemError ThreadStatusKind = "systemError"
)

// ProbeResult captures the narrow slice of `thread/read` we care about
// for on-reopen reconciliation. We deliberately don't surface the full
// Thread struct — the consumer only needs the status kind to decide
// the reconcile strategy; if future callers need more, widen this
// struct rather than returning the raw response.
type ProbeResult struct {
	// Status is the thread's runtime status. Never empty on a successful
	// probe — an unrecognised value from the wire is returned as its
	// literal string so the caller can fall back to "treat as systemError"
	// without letting typos silently classify as a known kind.
	Status ThreadStatusKind

	// ActiveFlags, when Status == ThreadStatusActive, enumerates what
	// the provider reports is in-flight (e.g., a running background
	// tool). Empty for non-active statuses.
	ActiveFlags []string
}

// Probe calls `thread/read { threadId, includeTurns: false }` on the
// Codex app-server and returns the thread's status kind. Used by the
// on-reopen reconciler to decide whether to keep the thread running
// (idle/active), re-hydrate it (notLoaded), or mark its running
// background tools as lost (systemError).
//
// Returns an error only on transport/decoding failure. An unknown
// `status.type` from the wire is NOT treated as an error; it's
// returned as-is so the caller can fall back to the conservative
// "treat as systemError" reconciliation.
func (s *Session) Probe(ctx context.Context) (ProbeResult, error) {
	// Tests install a probeFn override so reconcile paths can run
	// without a real app-server subprocess. Production Session
	// construction never sets it; the default wire path below is the
	// only branch that runs in production.
	if s.probeFn != nil {
		return s.probeFn(ctx)
	}
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return ProbeResult{}, fmt.Errorf("codex: probe: session has no thread id")
	}
	resp, err := s.sendRequest(ctx, "thread/read", map[string]any{
		"threadId":     rootThreadID,
		"includeTurns": false,
	})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("codex: thread/read: %w", classifyThreadWriterConflict(err))
	}

	return decodeProbeResponse(resp)
}

// Resume calls `thread/resume` on a live session. Used by the on-reopen
// reconciler when Probe reports `notLoaded` — the session is up but the
// thread isn't in memory, so the app-server needs to be told to rehydrate
// it before any further turn/start will work.
//
// This is NOT the resume path used by NewSession. NewSession calls
// thread/resume as part of its initial handshake to seed the session's
// root thread id.
// Resume here runs AFTER that handshake, on a session whose app-server
// has since forgotten the thread (e.g., provider crashed and auto-
// restarted, or idle eviction). We reuse the same wire method with the
// already-known thread id; the response body is ignored because the
// session already has everything it needs to dispatch turns.
//
// Returns an error only on transport/decoding failure. A session whose
// proc has gone away (Close already fired) errors out of sendRequest
// before it writes anything.
func (s *Session) Resume(ctx context.Context) error {
	// Tests install a resumeFn override so the reconcile path can run
	// without a real app-server subprocess (mirrors probeFn).
	// Production construction never sets it.
	if s.resumeFn != nil {
		return s.resumeFn(ctx)
	}
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return fmt.Errorf("codex: resume: session has no thread id")
	}
	_, err := s.sendRequest(ctx, "thread/resume", map[string]any{
		"threadId":     rootThreadID,
		"excludeTurns": true,
	})
	if err != nil {
		return fmt.Errorf("codex: thread/resume: %w", classifyThreadWriterConflict(err))
	}
	return nil
}

// decodeProbeResponse pulls `thread.status` out of the raw `thread/read`
// response. Exported (as package-private) so tests can feed crafted
// JSON without spinning up a full session.
func decodeProbeResponse(resp json.RawMessage) (ProbeResult, error) {
	var shape struct {
		Thread struct {
			Status struct {
				Type        string   `json:"type"`
				ActiveFlags []string `json:"activeFlags"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp, &shape); err != nil {
		return ProbeResult{}, fmt.Errorf("codex: decode thread/read response: %w", err)
	}
	if shape.Thread.Status.Type == "" {
		return ProbeResult{}, fmt.Errorf("codex: thread/read response missing thread.status.type")
	}
	return ProbeResult{
		Status:      ThreadStatusKind(shape.Thread.Status.Type),
		ActiveFlags: shape.Thread.Status.ActiveFlags,
	}, nil
}
