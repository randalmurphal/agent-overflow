package triage

import (
	"strings"

	"agent-overflow/internal/provider"
)

func rememberInteractiveRequestOrder(order map[string][]string, threadID, requestID string) {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if threadID == "" || requestID == "" {
		return
	}
	for _, existing := range order[threadID] {
		if existing == requestID {
			return
		}
	}
	order[threadID] = append(order[threadID], requestID)
}

func removeInteractiveRequestOrder(order map[string][]string, threadID, requestID string) {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	if threadID == "" || requestID == "" {
		return
	}
	ids := order[threadID]
	for i, existing := range ids {
		if existing != requestID {
			continue
		}
		ids = append(ids[:i], ids[i+1:]...)
		if len(ids) == 0 {
			delete(order, threadID)
			return
		}
		order[threadID] = ids
		return
	}
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
	if len(r.pendingApprovalOrder[threadID]) > 0 {
		return true
	}
	if len(r.pendingUserInputOrder[threadID]) > 0 {
		return true
	}
	if len(r.queuedFlushItems[threadID]) > 0 {
		return true
	}
	if len(r.pendingByThread[threadID]) > 0 {
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

	for _, requestID := range r.pendingApprovalOrder[threadID] {
		key := approvalStateKey(threadID, requestID)
		pending, ok := r.pendingApprovals[key]
		if !ok {
			continue
		}
		snapshot.Approvals = append(snapshot.Approvals, pending.Request)
	}

	for _, requestID := range r.pendingUserInputOrder[threadID] {
		key := approvalStateKey(threadID, requestID)
		request, ok := r.pendingUserInputs[key]
		if !ok {
			continue
		}
		snapshot.UserInputs = append(snapshot.UserInputs, request)
	}

	return snapshot
}
