package triage

import (
	"strings"

	"agent-overflow/internal/provider"
)

// rememberInteractiveRequestOrder appends requestID to one thread's
// display-order list, preserving first-seen order and ignoring repeats.
func rememberInteractiveRequestOrder(order []string, threadID, requestID string) []string {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if threadID == "" || requestID == "" {
		return order
	}
	for _, existing := range order {
		if existing == requestID {
			return order
		}
	}
	return append(order, requestID)
}

// removeInteractiveRequestOrder drops requestID from one thread's
// display-order list.
func removeInteractiveRequestOrder(order []string, threadID, requestID string) []string {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if threadID == "" || requestID == "" {
		return order
	}
	ids := order
	for i, existing := range ids {
		if existing != requestID {
			continue
		}
		ids = append(ids[:i], ids[i+1:]...)
		if len(ids) == 0 {
			return nil
		}
		return ids
	}
	return order
}

// HasPendingWork reports whether the router holds any user-blocking live
// state for threadID: pending approvals, pending user-input requests,
// queued flush items, or pending sends awaiting wire echo. The idle
// session reaper consults this to avoid killing sessions that the user
// perceives as active or blocked-on-user.
//
// All checks run under a single r.mu acquisition so no map can change
// between reads. Nil-safe: returns false when r is nil (test fixtures,
// partial init).
func (r *Router) HasPendingWork(threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	if len(st.pendingApprovalOrder) > 0 {
		return true
	}
	if len(st.pendingUserInputOrder) > 0 {
		return true
	}
	if len(st.queuedFlushItems) > 0 {
		return true
	}
	if len(st.pendingSends) > 0 {
		return true
	}
	return false
}

// PendingInteractiveRequests returns the still-open approval and structured
// input prompts for one live thread. It is intentionally runtime-only: if the
// provider process is gone, there is no valid request left to answer.
func (r *Router) PendingInteractiveRequests(threadID string) provider.PendingInteractiveRequests {
	snapshot := provider.PendingInteractiveRequests{
		Approvals:  []provider.ApprovalRequest{},
		UserInputs: []provider.UserInputRequest{},
	}
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return snapshot
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return snapshot
	}

	for _, requestID := range st.pendingApprovalOrder {
		pending, ok := st.pendingApprovals[requestID]
		if !ok {
			continue
		}
		snapshot.Approvals = append(snapshot.Approvals, pending.Request)
	}

	for _, requestID := range st.pendingUserInputOrder {
		request, ok := st.pendingUserInputs[requestID]
		if !ok {
			continue
		}
		snapshot.UserInputs = append(snapshot.UserInputs, request)
	}

	return snapshot
}
