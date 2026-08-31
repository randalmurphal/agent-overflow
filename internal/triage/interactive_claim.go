package triage

import "strings"

// Arbitration for an open question two screens can both answer.
//
// One backend now serves several clients, and each of them renders the same
// approval prompt and the same structured-input form. Two people — or one
// person on a laptop and a phone — can answer within the same second. Only one
// answer may reach the provider; the other must be told it arrived second, not
// handed a failure for a question that is no longer open.
//
// The router is the arbiter because it is the only thing that sees every
// answer for a thread. It records the request ids it has forwarded an answer
// for, and refuses the second. That closes both windows with one check: the
// SIMULTANEOUS one, where both calls arrive before either resolution lands,
// and the SEQUENTIAL one, where the second client is answering a card its
// screen has not dropped yet.
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

// ClaimApprovalResponse reports whether this caller may forward an approval
// answer. False means the prompt was already answered — by another client, or
// by an earlier call from this one.
func (r *Router) ClaimApprovalResponse(threadID, requestID string) bool {
	return r.claimInteractiveResponse(threadID, requestID)
}

// ClaimUserInputResponse is ClaimApprovalResponse for structured-input forms.
// The two families share one record: request ids are unique within a thread,
// and a caller answering the wrong family is refused by the provider anyway.
func (r *Router) ClaimUserInputResponse(threadID, requestID string) bool {
	return r.claimInteractiveResponse(threadID, requestID)
}

func (r *Router) claimInteractiveResponse(threadID, requestID string) bool {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if r == nil || threadID == "" || requestID == "" {
		// Nothing to arbitrate. A caller with no request id is not racing
		// anyone, and refusing it here would turn a malformed call into a
		// confusing "someone else answered".
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if _, answered := st.answeredRequests[requestID]; answered {
		return false
	}
	if st.answeredRequests == nil {
		st.answeredRequests = make(map[string]struct{})
	}
	st.answeredRequests[requestID] = struct{}{}
	return true
}

// ReleaseInteractiveResponse withdraws a claim whose answer never reached the
// provider. Without it a failed answer would wedge the prompt: the request is
// still open, still rendered, and every later attempt — including a retry from
// the same client — would be refused as already handled. Idempotent.
func (r *Router) ReleaseInteractiveResponse(threadID, requestID string) {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if r == nil || threadID == "" || requestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return
	}
	delete(st.answeredRequests, requestID)
}
