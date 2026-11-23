package codex

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const (
	maxDeferredChildEventsPerThread = 128
	maxDeferredChildEventsTotal     = 512
	maxDeferredChildEventBytes      = 4 * 1024 * 1024
	maxDeferredChildThreadIDBytes   = 256
	maxRememberedUnrelatedThreads   = 256
)

// deferredChildOwnershipTimeout is how long a child thread's wire events stay
// quarantined before the routing deadline rejects them. A var only so a test
// can shrink it; nothing outside a test may write it.
var deferredChildOwnershipTimeout = 10 * time.Second

// deferredChildWireEvent is either a notification (RequestID empty) or a
// server request. Server requests must be retained as well as notifications:
// a fast child can request approval before MultiAgentV2's parent-side
// subAgentActivity item establishes its spawn ownership.
type deferredChildWireEvent struct {
	Method    string
	Params    json.RawMessage
	RequestID string
	RawLine   json.RawMessage
}

func (e deferredChildWireEvent) sizeBytes() int {
	return len(e.Method) + len(e.Params) + len(e.RequestID) + len(e.RawLine)
}

type childWireRoute uint8

const (
	childWireRoutable childWireRoute = iota
	childWireUnrelated
	childWireDeferred
	childWireOverflow
)

func (s *Session) isUnrelatedProviderThread(providerThreadID string) bool {
	if providerThreadID == "" || providerThreadID == s.rootThreadID() {
		return false
	}
	s.mu.Lock()
	_, unrelated := s.childRouting.unrelatedThreads[providerThreadID]
	if s.collab.childParentByThread[providerThreadID] != "" {
		unrelated = false
	}
	s.mu.Unlock()
	return unrelated
}

// Caller holds mu. Corrections remove the ID from both structures so a later
// insertion cannot leave a stale queue entry that evicts the new identity.
func (s *Session) forgetUnrelatedProviderThreadLocked(providerThreadID string) bool {
	if _, exists := s.childRouting.unrelatedThreads[providerThreadID]; !exists {
		return false
	}
	delete(s.childRouting.unrelatedThreads, providerThreadID)
	for i, id := range s.childRouting.unrelatedOrder {
		if id == providerThreadID {
			copy(s.childRouting.unrelatedOrder[i:], s.childRouting.unrelatedOrder[i+1:])
			s.childRouting.unrelatedOrder[len(s.childRouting.unrelatedOrder)-1] = ""
			s.childRouting.unrelatedOrder = s.childRouting.unrelatedOrder[:len(s.childRouting.unrelatedOrder)-1]
			return true
		}
	}
	log.Printf("codex: unrelated thread %s missing from bounded order", providerThreadID)
	return true
}

// A different, explicitly reported sessionId proves this thread is outside
// the root's agent tree. Absence is not proof: older or partial metadata must
// stay quarantined until a typed spawn owns it or the deadline warns.
func (s *Session) discardUnrelatedProviderThread(providerThreadID, sessionID, parentThreadID string) bool {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" || len(providerThreadID) > maxDeferredChildThreadIDBytes || providerThreadID == s.rootThreadID() {
		return false
	}
	s.mu.Lock()
	rootSessionID := s.rootSessionID()
	linked := s.collab.childParentByThread[providerThreadID] != "" ||
		parentThreadID == s.rootThreadID() ||
		s.collab.childParentByThread[parentThreadID] != ""
	if linked || (rootSessionID != "" && sessionID == rootSessionID) {
		wasUnrelated := s.forgetUnrelatedProviderThreadLocked(providerThreadID)
		s.mu.Unlock()
		if wasUnrelated {
			log.Printf("codex: corrected session identity for thread %s; retaining child routing", providerThreadID)
		} else if linked && rootSessionID != "" && sessionID != "" && sessionID != rootSessionID {
			log.Printf("codex: conflicting session identity for child %s: root=%s reported=%s parent=%s; retaining child routing", providerThreadID, rootSessionID, sessionID, parentThreadID)
		}
		return false
	}
	if _, unrelated := s.childRouting.unrelatedThreads[providerThreadID]; unrelated {
		s.mu.Unlock()
		return true
	}
	if rootSessionID == "" || sessionID == "" {
		s.mu.Unlock()
		return false
	}
	if s.childRouting.unrelatedThreads == nil {
		s.childRouting.unrelatedThreads = make(map[string]struct{})
	}
	if _, exists := s.childRouting.unrelatedThreads[providerThreadID]; !exists {
		if len(s.childRouting.unrelatedOrder) < maxRememberedUnrelatedThreads {
			s.childRouting.unrelatedOrder = append(s.childRouting.unrelatedOrder, providerThreadID)
		} else {
			old := s.childRouting.unrelatedOrder[0]
			delete(s.childRouting.unrelatedThreads, old)
			copy(s.childRouting.unrelatedOrder, s.childRouting.unrelatedOrder[1:])
			s.childRouting.unrelatedOrder[len(s.childRouting.unrelatedOrder)-1] = providerThreadID
		}
		s.childRouting.unrelatedThreads[providerThreadID] = struct{}{}
	}
	events := s.takeDeferredChildWireEventsLocked(providerThreadID)
	s.mu.Unlock()

	for _, event := range events {
		if event.RequestID != "" {
			s.rejectUnrelatedRequest(event.RequestID)
		}
	}
	return true
}

func (s *Session) rejectUnrelatedRequest(requestID string) {
	rpcID, err := json.Number(requestID).Int64()
	if err != nil {
		log.Printf("codex: reject unrelated thread request id %q: %v", requestID, err)
		return
	}
	if err := s.writeErrorResponse(rpcID, -32000, "thread belongs to another Codex session"); err != nil {
		log.Printf("codex: reject unrelated thread request %d: %v", rpcID, err)
	}
}

// routeChildWireEvent makes the ownership decision and queue insertion under
// one lock. A recovery worker may register a spawn concurrently with readLoop;
// separate checks could enqueue an event after the spawn drained its queue.
func (s *Session) routeChildWireEvent(providerThreadID string, event deferredChildWireEvent) childWireRoute {
	providerThreadID = strings.TrimSpace(providerThreadID)
	rootThreadID := strings.TrimSpace(s.rootThreadID())
	if providerThreadID == "" || rootThreadID == "" || providerThreadID == rootThreadID {
		return childWireRoutable
	}
	if len(providerThreadID) > maxDeferredChildThreadIDBytes {
		return childWireOverflow
	}
	event.Method = strings.TrimSpace(event.Method)
	eventBytes := event.sizeBytes()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collab.childParentByThread[providerThreadID] != "" && !s.childRouting.backlogOpen[providerThreadID] {
		return childWireRoutable
	}
	if _, unrelated := s.childRouting.unrelatedThreads[providerThreadID]; unrelated {
		return childWireUnrelated
	}
	if s.childRouting.deferredChildWireEvents == nil {
		s.childRouting.deferredChildWireEvents = make(map[string][]deferredChildWireEvent)
	}
	queuedForThread := s.childRouting.deferredChildWireEvents[providerThreadID]
	if len(queuedForThread) >= maxDeferredChildEventsPerThread ||
		s.childRouting.deferredChildWireCount >= maxDeferredChildEventsTotal ||
		eventBytes > maxDeferredChildEventBytes ||
		s.childRouting.deferredChildWireBytes > maxDeferredChildEventBytes-eventBytes {
		return childWireOverflow
	}
	event.Params = append(json.RawMessage(nil), event.Params...)
	event.RawLine = append(json.RawMessage(nil), event.RawLine...)
	s.childRouting.deferredChildWireEvents[providerThreadID] = append(queuedForThread, event)
	s.childRouting.deferredChildWireCount++
	s.childRouting.deferredChildWireBytes += eventBytes
	if s.childRouting.deferredChildDeadlines == nil {
		s.childRouting.deferredChildDeadlines = make(map[string]*time.Timer)
	}
	if s.childRouting.deferredChildDeadlines[providerThreadID] == nil {
		threadID := providerThreadID
		s.childRouting.deferredChildDeadlines[providerThreadID] = time.AfterFunc(deferredChildOwnershipTimeout, func() {
			// The expiry writes a JSON-RPC rejection and can emit a routing
			// warning, so it has to be OVER before Close returns — and
			// Close's own timer.Stop() cannot promise that: Stop does not
			// wait for a callback already running, and it only runs after
			// the drains it would be racing. Registering the work with
			// collabAsyncWG (which Close waits on, having already latched
			// collabAsyncClosing) is what closes both halves: a timer that
			// fires after the latch is refused outright, and one that beat
			// it is joined.
			s.startCollabAsync(func() {
				s.expireDeferredChildWireEvents(threadID)
			})
		})
	}
	return childWireDeferred
}

// takeDeferredChildWireEventsUnlessClosing removes and returns one child
// thread's quarantined queue, cancelling its deadline timer.
//
// stopIfClosing is for the EXPIRY path, and it must be answered under mu
// rather than by the caller afterwards: taking the queue and then noticing
// the session is closing drops the events either way, but it does so having
// already committed to a rejection the caller then never writes. Under mu the
// two are one decision — Close's own drain sees an untouched queue and logs
// it as unresolved ownership, which is what it is.
func (s *Session) takeDeferredChildWireEventsUnlessClosing(providerThreadID string, stopIfClosing bool) []deferredChildWireEvent {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if stopIfClosing && s.closing.Load() {
		return nil
	}
	return s.takeDeferredChildWireEventsLocked(providerThreadID)
}

// Caller holds mu. Keeping removal under the classification lock prevents a
// concurrent typed spawn from claiming a queue after its route was discarded.
func (s *Session) takeDeferredChildWireEventsLocked(providerThreadID string) []deferredChildWireEvent {
	events := s.childRouting.deferredChildWireEvents[providerThreadID]
	if len(events) == 0 {
		return nil
	}
	delete(s.childRouting.deferredChildWireEvents, providerThreadID)
	delete(s.childRouting.recoveryPending, providerThreadID)
	if timer := s.childRouting.deferredChildDeadlines[providerThreadID]; timer != nil {
		timer.Stop()
		delete(s.childRouting.deferredChildDeadlines, providerThreadID)
	}
	for _, event := range events {
		s.childRouting.deferredChildWireBytes -= event.sizeBytes()
	}
	s.childRouting.deferredChildWireCount -= len(events)
	if s.childRouting.deferredChildWireCount < 0 {
		log.Printf("codex: deferred child routing event count underflow; resetting")
		s.childRouting.deferredChildWireCount = 0
	}
	if s.childRouting.deferredChildWireBytes < 0 {
		// Defensive only: every queue mutation is under mu, so reaching this
		// branch indicates an accounting bug rather than provider input.
		log.Printf("codex: deferred child routing byte count underflow; resetting")
		s.childRouting.deferredChildWireBytes = 0
	}
	return events
}

func (s *Session) expireDeferredChildWireEvents(providerThreadID string) {
	events := s.takeDeferredChildWireEventsUnlessClosing(providerThreadID, true)
	if len(events) == 0 {
		return
	}
	s.deleteUnownedAgentMeta(providerThreadID)
	for _, event := range events {
		if event.RequestID == "" {
			continue
		}
		rpcID, err := json.Number(event.RequestID).Int64()
		if err != nil {
			log.Printf("codex: reject expired child request id %q: %v", event.RequestID, err)
			continue
		}
		if err := s.writeErrorResponse(rpcID, -32000, "subagent ownership did not arrive before the routing deadline"); err != nil {
			log.Printf("codex: reject expired child request %d: %v", rpcID, err)
		}
	}
	s.warnChildRoutingOverflow(providerThreadID, "ownership timeout", nil)
}

// drainDeferredChildWireEvents delivers each child's quarantined frames in
// wire order, once the child's route is registered. A frame that arrives
// while a gated backlog is delivered (gateChildBacklog) joins the queue, so
// the loop takes the queue again until it is empty and opens the route in
// the same step. A drained frame already passed the checks dispatch runs
// before routing, and its route is known, so it goes straight to the
// routed dispatch: through the gate it would only be queued again.
func (s *Session) drainDeferredChildWireEvents(providerThreadIDs ...string) {
	for _, providerThreadID := range providerThreadIDs {
		providerThreadID = strings.TrimSpace(providerThreadID)
		needsFlush := false
		if s.conn != nil && s.conn.registerChildRoute(providerThreadID, s) {
			needsFlush = true
		}
		s.mu.Lock()
		if events := s.takeDeferredChildWireEventsLocked(providerThreadID); len(events) > 0 {
			s.enqueueEarlyFrames(events)
			needsFlush = true
		}
		delete(s.childRouting.backlogOpen, providerThreadID)
		s.mu.Unlock()
		if needsFlush {
			s.flushEarlyFrames()
		}
	}
}

// releaseChildBacklogGate opens a gate whose route was never registered:
// the child's frames stay quarantined until their ownership deadline, as
// any unowned child's do.
func (s *Session) releaseChildBacklogGate(providerThreadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.childRouting.backlogOpen, providerThreadID)
}

// gateChildBacklog holds a child's new frames in its quarantine queue until
// drainDeferredChildWireEvents has delivered the frames queued before them.
// A recovery that publishes a route off the read loop calls it before the
// route exists: otherwise the read loop routes a later frame (a command's
// completion, the thread's notLoaded) ahead of the backlog that holds the
// command's start.
func (s *Session) gateChildBacklog(providerThreadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childRouting.backlogOpen == nil {
		s.childRouting.backlogOpen = make(map[string]bool)
	}
	s.childRouting.backlogOpen[providerThreadID] = true
}

func (s *Session) warnChildRoutingOverflow(providerThreadID, method string, requestID *json.Number) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if requestID != nil {
		if rpcID, err := requestID.Int64(); err == nil {
			if writeErr := s.writeErrorResponse(rpcID, -32000, "subagent ownership was not available before the routing buffer filled"); writeErr != nil {
				log.Printf("codex: reject unroutable child request %s: %v", requestID.String(), writeErr)
			}
		}
	}

	s.mu.Lock()
	warned := s.childRouting.warned
	s.childRouting.warned = true
	s.mu.Unlock()
	if warned || s.closing.Load() {
		return
	}

	displayThreadID := providerThreadID
	if len(displayThreadID) > 80 {
		displayThreadID = displayThreadID[:80] + "…"
	}
	message := fmt.Sprintf("Codex child thread %s could not be matched to a spawn before the routing buffer closed; child events were dropped", displayThreadID)
	log.Printf("codex: %s (method=%s)", message, method)
	if s.onEvent == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"kind":             "warning",
		"title":            "Subagent routing warning",
		"providerThreadId": providerThreadID,
		"method":           method,
	})
	if err != nil {
		meta = json.RawMessage(`{"kind":"warning","title":"Subagent routing warning"}`)
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) dispatchServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	var reqID string
	if id != nil {
		reqID = id.String()
	}
	if s.enqueueFrameIfPending(deferredChildWireEvent{
		Method:    method,
		RequestID: reqID,
		Params:    params,
		RawLine:   line,
	}) {
		return
	}
	s.dispatchServerRequestDirect(method, id, params, line)
}

func (s *Session) dispatchServerRequestDirect(method string, id *json.Number, params json.RawMessage, line []byte) {
	providerThreadID := providerThreadIDFromParams(params)
	requestID := ""
	if id != nil {
		requestID = id.String()
	}
	switch s.routeChildWireEvent(providerThreadID, deferredChildWireEvent{
		Method:    method,
		Params:    params,
		RequestID: requestID,
		RawLine:   line,
	}) {
	case childWireUnrelated:
		if id != nil {
			s.rejectUnrelatedRequest(requestID)
		}
		return
	case childWireRoutable:
		s.handleServerRequest(method, id, params, line)
		return
	case childWireDeferred:
		return
	case childWireOverflow:
		s.warnChildRoutingOverflow(providerThreadID, method, id)
	default:
		panic("codex: invalid child wire route")
	}
}
