package codex

import (
	"context"
	"encoding/json"
	"fmt"
)

// ThreadStatusKind enumerates the closed set of `thread.status.type`
// values the Codex app-server reports on `thread/read`. The app relies
// on this enum to pick a reconciliation strategy after a restart — see
// `(*App).reconcileCodexOnReopen` for the consumer.
//
// Source of truth:
//   codex-rs/app-server-protocol/schema/typescript/v2/ThreadStatus.ts
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
	if s.codexThreadID == "" {
		return ProbeResult{}, fmt.Errorf("codex: probe: session has no thread id")
	}
	resp, err := s.sendRequest(ctx, "thread/read", map[string]any{
		"threadId":     s.codexThreadID,
		"includeTurns": false,
	})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("codex: thread/read: %w", err)
	}

	return decodeProbeResponse(resp)
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
