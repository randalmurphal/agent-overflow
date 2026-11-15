package triage

import (
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
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
	if identity := r.identityIfPresent(threadID); identity != nil && identity.pendingEchoes > 0 {
		return true
	}
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

// PromptRef names one prompt. A request id does not: a session can ask
// again under an id it resolved, and a replacement session under its
// predecessor's. The occurrence does (provider.ApprovalRequest.Occurrence).
type PromptRef struct {
	ThreadID, RequestID string
	Occurrence          uint64
}

// openPromptLocked finds the prompt open under threadID and requestID: its
// route, occurrence and the session that asked. Caller holds r.mu.
func (r *Router) openPromptLocked(threadID, requestID string) (route *provider.PromptRoute, occurrence uint64, session string, open bool) {
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return nil, 0, "", false
	}
	if pending, ok := st.pendingApprovals[requestID]; ok {
		return pending.Request.RoutedTo, pending.Request.Occurrence, pending.Request.SessionToken, true
	}
	if request, ok := st.pendingUserInputs[requestID]; ok {
		return request.RoutedTo, request.Occurrence, request.SessionToken, true
	}
	return nil, 0, "", false
}

// PromptRoute reports whether the prompt ref names is still waiting for an
// answer, and the thread it is routed to: nil when it is the user's. A
// prompt of a session that no longer holds its thread is not open.
func (r *Router) PromptRoute(ref PromptRef) (*provider.PromptRoute, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	route, open := r.promptRouteLocked(ref)
	if route == nil {
		return nil, open
	}
	copied := *route
	return &copied, open
}

// promptRouteLocked is PromptRoute under r.mu, returning the held route.
func (r *Router) promptRouteLocked(ref PromptRef) (*provider.PromptRoute, bool) {
	route, occurrence, session, open := r.openPromptLocked(ref.ThreadID, ref.RequestID)
	if !open || occurrence != ref.Occurrence || r.frameRetiredLocked(ref.ThreadID, session) {
		return nil, false
	}
	return route, true
}

// ReroutePrompts moves open prompts routed to from, which can no longer
// take them, to where the prompt router routes their threads now; one it
// would route to from again goes to the user. refs narrows it to those
// prompts, and none means every prompt routed to from. Each prompt that
// moved is announced again with its new route, so every client and the
// app's delivery follow it, and one handed to the user raises its
// notification. It returns how many moved.
func (r *Router) ReroutePrompts(from string, refs ...PromptRef) int {
	if r == nil || from == "" {
		return 0
	}
	routedFrom := func(route *provider.PromptRoute) bool { return route != nil && route.ThreadID == from }
	var moving []PromptRef
	r.mu.Lock()
	if len(refs) > 0 {
		for _, ref := range refs {
			if route, open := r.promptRouteLocked(ref); open && routedFrom(route) {
				moving = append(moving, ref)
			}
		}
	} else {
		for threadID, st := range r.threads {
			for requestID, pending := range st.pendingApprovals {
				if routedFrom(pending.Request.RoutedTo) {
					moving = append(moving, PromptRef{ThreadID: threadID, RequestID: requestID, Occurrence: pending.Request.Occurrence})
				}
			}
			for requestID, request := range st.pendingUserInputs {
				if routedFrom(request.RoutedTo) {
					moving = append(moving, PromptRef{ThreadID: threadID, RequestID: requestID, Occurrence: request.Occurrence})
				}
			}
		}
	}
	r.mu.Unlock()
	if len(moving) == 0 {
		return 0
	}
	// The router reads the store, so it runs outside r.mu. A prompt whose
	// route changed meanwhile is left to whatever changed it.
	routes := make(map[string]*provider.PromptRoute, len(moving))
	for _, ref := range moving {
		if _, done := routes[ref.ThreadID]; done {
			continue
		}
		route := r.routePrompt(ref.ThreadID)
		if routedFrom(route) {
			route = nil
		}
		routes[ref.ThreadID] = route
	}
	var approvals []provider.ApprovalEvent
	var questions []provider.UserInputEvent
	now := time.Now().UnixMilli()
	r.mu.Lock()
	for _, ref := range moving {
		if route, open := r.promptRouteLocked(ref); !open || !routedFrom(route) {
			continue
		}
		var route *provider.PromptRoute
		if to := routes[ref.ThreadID]; to != nil {
			copied := *to
			route = &copied
		}
		st := r.threadStateIfPresent(ref.ThreadID)
		if pending, ok := st.pendingApprovals[ref.RequestID]; ok {
			pending.Request.RoutedTo = route
			st.pendingApprovals[ref.RequestID] = pending
			request := pending.Request
			approvals = append(approvals, provider.ApprovalEvent{Action: "request", ThreadID: ref.ThreadID, Request: &request, RequestedAt: now})
			continue
		}
		request := st.pendingUserInputs[ref.RequestID]
		request.RoutedTo = route
		st.pendingUserInputs[ref.RequestID] = request
		questions = append(questions, provider.UserInputEvent{Action: "request", ThreadID: ref.ThreadID, Request: &request, RequestedAt: now})
	}
	r.mu.Unlock()
	for _, event := range approvals {
		r.emit(eventchan.ProviderApproval, event)
	}
	for _, event := range questions {
		r.emit(eventchan.ProviderUserInput, event)
	}
	return len(approvals) + len(questions)
}
