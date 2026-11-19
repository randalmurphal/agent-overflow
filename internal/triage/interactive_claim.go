package triage

import (
	"strings"

	"agent-overflow/internal/provider"
)

// Arbitration for an open question two screens can both answer.
//
// One backend now serves several clients, and each of them renders the same
// approval prompt and the same structured-input form. Two people — or one
// person on a laptop and a phone — can answer within the same second. Only one
// answer may reach the provider; the other must be told it arrived second, not
// handed a failure for a question that is no longer open.
//
// The router is the arbiter because it is the only thing that sees every
// answer for a thread. It records, by request id, the prompt occurrence it has
// forwarded an answer for, and refuses the second. That closes both windows
// with one check: the SIMULTANEOUS one, where both calls arrive before either
// resolution lands, and the SEQUENTIAL one, where the second client is
// answering a card its screen has not dropped yet. Each client's answer names
// the occurrence its card showed, so a late answer to a prompt that was
// answered or ended is refused even after the provider asked again under the
// same request id.
//
// It refuses only on POSITIVE evidence — an answer this router forwarded. A
// request it has no record of is passed through rather than refused: the
// router's pending map is not the only authority on what is answerable (the
// Codex session keeps its own request table and answers an untracked id with
// provider.ErrStaleInteractiveRequest), and reporting "someone else answered"
// about a request nobody answered would be a worse lie than the one it fixes.
//
// The record is deliberately NOT the pending entry itself. handleApprovalResolved
// reads that entry to build the resolved tool row's summary, so consuming it
// here would leave the row describing the wrong input.

// PromptClaim is the right to forward one answer: the prompt it answers
// and the only session the answer may be written to. Ref.Occurrence is
// zero, and SessionToken empty, for a request this router holds no prompt
// for.
type PromptClaim struct {
	Ref          PromptRef
	SessionToken string
}

// ClaimPrompt reports whether this caller may forward an answer to the
// prompt open under threadID and requestID. occurrence names the prompt
// the caller was shown; zero, from a client that predates the occurrence,
// means whichever is open now. False means that prompt was already answered, by
// another client or by an earlier call from this one, or, for a named
// occurrence, that it is no longer open: a later prompt under the same
// request id is not the one the caller was shown. Approvals and questions
// share one record: request ids are unique within a thread.
func (r *Router) ClaimPrompt(threadID, requestID string, occurrence provider.PromptOccurrence) (PromptClaim, bool) {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	claim := PromptClaim{Ref: PromptRef{ThreadID: threadID, RequestID: requestID}}
	if r == nil || threadID == "" || requestID == "" {
		// Nothing to arbitrate. A caller with no request id is not racing
		// anyone, and refusing it here would turn a malformed call into a
		// confusing "someone else answered".
		return claim, occurrence == 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, open, session, isOpen := r.openPromptLocked(threadID, requestID)
	if occurrence != 0 && (!isOpen || open != occurrence || r.frameRetiredLocked(threadID, session)) {
		return claim, false
	}
	if isOpen {
		claim.Ref.Occurrence, claim.SessionToken = open, session
	}
	st := r.state(threadID)
	// A request with no prompt open is refused on any record: its prompt
	// was answered and then resolved, or its agent ended.
	if held, answered := st.answeredRequests[requestID]; answered && (held == claim.Ref.Occurrence || !isOpen) {
		return claim, false
	}
	if st.answeredRequests == nil {
		st.answeredRequests = make(map[string]provider.PromptOccurrence)
	}
	st.answeredRequests[requestID] = claim.Ref.Occurrence
	return claim, true
}

// ReleasePrompt withdraws a claim whose answer never reached the provider.
// Without it a failed answer would wedge the prompt: the request is still
// open, still rendered, and every later attempt, including a retry from the
// same client, would be refused as already handled. It releases only that
// claim: a later prompt's claim under the same request id is not its.
// Idempotent.
func (r *Router) ReleasePrompt(claim PromptClaim) {
	ref := claim.Ref
	if r == nil || ref.ThreadID == "" || ref.RequestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(ref.ThreadID)
	if st == nil {
		return
	}
	if held, answered := st.answeredRequests[ref.RequestID]; answered && held == ref.Occurrence {
		delete(st.answeredRequests, ref.RequestID)
	}
}
