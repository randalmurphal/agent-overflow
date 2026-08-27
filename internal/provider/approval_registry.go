package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
)

var ErrApprovalDecisionUnavailable = errors.New("provider: approval decision is not available")

// ResolvedApproval is one interactive request a Claim, Cancel or Drain
// released from the registry — the request id the frontend keyed its prompt
// panel on, plus the resolve event kind that prompt is listening for.
//
// It is what the registry hands back instead of emitting: the shared code
// owns "which requests are outstanding and who gets to answer each one", and
// each provider owns what happens on the way out (Codex writes a JSON-RPC
// error to unblock the server request; Claude writes nothing, because the CLI
// has already stopped awaiting).
type ResolvedApproval struct {
	RequestID   string
	ResolveKind EventKind
}

// Meta builds the resolved-event meta both providers emit for a released
// request. `decision` is the word the frontend and triage branch on —
// "lost" for a session that died mid-prompt (triage flips the row to
// errored), "cancel" for a request the provider itself abandoned.
//
// A user-input resolution additionally carries an empty `answers` map: the
// frontend types answers as present on every UserInputResolved event, so
// omitting it on the no-reply path would break the contract for exactly the
// case where no reply exists.
func (r ResolvedApproval) Meta(decision string) json.RawMessage {
	fields := map[string]any{
		"requestId": r.RequestID,
		"decision":  decision,
	}
	if r.ResolveKind == EventUserInputResolved {
		fields["answers"] = map[string]any{}
	}
	meta, _ := json.Marshal(fields)
	return meta
}

// pendingApproval is one outstanding request's registry state.
type pendingApproval struct {
	resolveKind EventKind
	// questions is the structured user-input prompt this request carries,
	// needed when the answer is submitted so the response can be shaped
	// against the questions actually asked. Claude-only: Codex re-reads its
	// questions off the wire params. Stored as an owned copy.
	questions []UserInputQuestion
	// allowedDecisions is nil when the provider did not advertise a set.
	// Keys are compact JSON, which makes object member whitespace and caller
	// formatting irrelevant while preserving the provider's exact values.
	allowedDecisions map[string]struct{}
}

// ApprovalRegistry is the per-session ledger of outstanding interactive
// requests (tool approvals and structured user input), shared by both
// providers. It answers exactly one question — who is allowed to resolve
// request X, and once — and deliberately answers no others: it never emits,
// never writes to a provider, and never knows a thread id.
//
// That boundary is what keeps it usable from both packages, and it is also
// the lock discipline. The registry's mutex is a LEAF: nothing it does can
// reach back into a Session, so a caller may hold a session lock across a
// registry call, and the registry can never be holding its own lock while an
// emission or a provider write runs. Drain RETURNS the released entries
// rather than resolving them precisely so the emit happens after the lock is
// gone — see codex's session lock order, where `drainPendingApprovals`
// releasing before it emits is a documented requirement.
//
// The zero value is ready to use — the dedup set falls back to
// DefaultApprovalDedupCap, which is what both providers take.
type ApprovalRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
	dedup   ApprovalDeduper
	closed  bool
}

// Track registers an outstanding request. An empty id is ignored, and so is
// every request arriving after Drain closed the registry — a prompt
// registered after teardown could never be answered or drained, so it would
// hang the frontend panel for the rest of the session.
//
// questions may be nil. Re-registering an id FORGETS its dedup entry: a
// provider that re-sends a request it previously answered is asking again,
// and refusing the new one as a duplicate would strand it.
func (r *ApprovalRegistry) Track(requestID string, resolveKind EventKind, questions []UserInputQuestion) {
	r.track(requestID, resolveKind, questions, nil)
}

// TrackApproval registers a tool approval and its optional provider-advertised
// decision set. nil means legacy fallback. A non-nil empty slice allows no
// response, matching the provider's authoritative empty set.
func (r *ApprovalRegistry) TrackApproval(requestID string, resolveKind EventKind, decisions *[]json.RawMessage) {
	var allowed map[string]struct{}
	if decisions != nil {
		allowed = make(map[string]struct{}, len(*decisions))
		for _, decision := range *decisions {
			if normalized, ok := normalizeApprovalDecisionJSON(decision); ok {
				allowed[normalized] = struct{}{}
			}
		}
	}
	r.track(requestID, resolveKind, nil, allowed)
}

func (r *ApprovalRegistry) track(requestID string, resolveKind EventKind, questions []UserInputQuestion, allowed map[string]struct{}) {
	if requestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.pending == nil {
		r.pending = make(map[string]*pendingApproval)
	}
	r.pending[requestID] = &pendingApproval{
		resolveKind:      resolveKind,
		questions:        append([]UserInputQuestion(nil), questions...),
		allowedDecisions: allowed,
	}
	r.dedup.Forget(requestID)
}

// ClaimApproval atomically validates and takes a tool approval. Advertised
// decisions are enforced in the backend so a stale or hand-crafted frontend
// response cannot submit a choice the server excluded.
func (r *ApprovalRegistry) ClaimApproval(requestID string, decision json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedup.IsResolved(requestID) {
		return ErrStaleInteractiveRequest
	}
	pending, ok := r.pending[requestID]
	if !ok || pending.resolveKind != EventApprovalResolved {
		return ErrStaleInteractiveRequest
	}
	if pending.allowedDecisions != nil {
		normalized, valid := normalizeApprovalDecisionJSON(decision)
		if !valid {
			return ErrApprovalDecisionUnavailable
		}
		if _, allowed := pending.allowedDecisions[normalized]; !allowed {
			return ErrApprovalDecisionUnavailable
		}
	}
	delete(r.pending, requestID)
	r.dedup.MarkResolved(requestID)
	return nil
}

func normalizeApprovalDecisionJSON(decision json.RawMessage) (string, bool) {
	decision = bytes.TrimSpace(decision)
	if len(decision) == 0 || !json.Valid(decision) {
		return "", false
	}
	var value any
	if json.Unmarshal(decision, &value) != nil {
		return "", false
	}
	normalized, err := json.Marshal(value)
	return string(normalized), err == nil
}

// Questions returns an owned copy of the structured questions registered with
// requestID, or nil when there are none.
func (r *ApprovalRegistry) Questions(requestID string) []UserInputQuestion {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.pending[requestID]
	if pending == nil || len(pending.questions) == 0 {
		return nil
	}
	return append([]UserInputQuestion(nil), pending.questions...)
}

// Claim reports whether the caller is the first to answer requestID, and
// takes the request when it is. False means the request was already resolved,
// was never registered, or is of a different kind than the caller expects —
// a user-input answer must not resolve a tool approval that happens to share
// an id. Callers surface it as a stale-request error rather than writing a
// second response that would shadow the first.
func (r *ApprovalRegistry) Claim(requestID string, expectedKind EventKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedup.IsResolved(requestID) {
		return false
	}
	pending, ok := r.pending[requestID]
	if !ok || pending.resolveKind != expectedKind {
		return false
	}
	delete(r.pending, requestID)
	r.dedup.MarkResolved(requestID)
	return true
}

// Cancel takes a request the PROVIDER abandoned (Claude's
// control_cancel_request after an interrupt), returning what the caller needs
// to emit the matching resolved event. Idempotent: an already-resolved or
// unknown id returns false and changes nothing.
//
// Unlike Claim it does not check the kind — the provider is retracting the
// request whatever it was, so there is no caller expectation to disagree with.
func (r *ApprovalRegistry) Cancel(requestID string) (ResolvedApproval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedup.IsResolved(requestID) {
		return ResolvedApproval{}, false
	}
	pending, ok := r.pending[requestID]
	if !ok {
		return ResolvedApproval{}, false
	}
	delete(r.pending, requestID)
	r.dedup.MarkResolved(requestID)
	return ResolvedApproval{RequestID: requestID, ResolveKind: pending.resolveKind}, true
}

// Drain takes every outstanding request and returns them for the caller to
// resolve. The registry is empty afterwards, so a second Drain returns
// nothing and cannot double-resolve.
//
// closeRegistry=true is the session-close path: it latches the registry shut
// (Track refuses from here on) and drops the dedup set, which is safe only
// because no duplicate response can reach a provider that is going away.
// closeRegistry=false is a mid-life drain — Codex's interrupt, which
// abandons the turn's prompts while the session keeps running and may
// register new ones immediately.
//
// Order is map order, i.e. unspecified. Nothing may depend on it: each
// released request resolves independently and the frontend keys on request id.
func (r *ApprovalRegistry) Drain(closeRegistry bool) []ResolvedApproval {
	r.mu.Lock()
	if closeRegistry {
		r.closed = true
		r.dedup.Reset()
	}
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}
	released := make([]ResolvedApproval, 0, len(pending))
	for requestID, p := range pending {
		released = append(released, ResolvedApproval{RequestID: requestID, ResolveKind: p.resolveKind})
	}
	return released
}
