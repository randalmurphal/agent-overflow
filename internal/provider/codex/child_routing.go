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

// isUnmappedForeignProviderThread is the fail-closed boundary for app-server
// multiplexing. Once the root provider thread is known, any other thread must
// have a typed spawn ownership edge before its events may enter AO's parent
// projection. This applies recursively: a grandchild is foreign until the
// subAgentActivity emitted on its child parent maps it to the nested spawn row.
func (s *Session) isUnmappedForeignProviderThread(providerThreadID string) bool {
	providerThreadID = strings.TrimSpace(providerThreadID)
	rootThreadID := strings.TrimSpace(s.rootThreadID())
	if providerThreadID == "" || rootThreadID == "" || providerThreadID == rootThreadID {
		return false
	}
	return s.parentToolUseForProviderThread(providerThreadID) == ""
}

func (s *Session) deferChildWireEvent(providerThreadID string, event deferredChildWireEvent) bool {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" || len(providerThreadID) > maxDeferredChildThreadIDBytes {
		return false
	}
	event.Method = strings.TrimSpace(event.Method)
	eventBytes := event.sizeBytes()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childRouting.deferredChildWireEvents == nil {
		s.childRouting.deferredChildWireEvents = make(map[string][]deferredChildWireEvent)
	}
	queuedForThread := s.childRouting.deferredChildWireEvents[providerThreadID]
	if len(queuedForThread) >= maxDeferredChildEventsPerThread ||
		s.childRouting.deferredChildWireCount >= maxDeferredChildEventsTotal ||
		eventBytes > maxDeferredChildEventBytes ||
		s.childRouting.deferredChildWireBytes > maxDeferredChildEventBytes-eventBytes {
		return false
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
	return true
}

func (s *Session) takeDeferredChildWireEvents(providerThreadID string) []deferredChildWireEvent {
	return s.takeDeferredChildWireEventsUnlessClosing(providerThreadID, false)
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
	events := s.childRouting.deferredChildWireEvents[providerThreadID]
	if len(events) == 0 {
		return nil
	}
	delete(s.childRouting.deferredChildWireEvents, providerThreadID)
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

func (s *Session) drainDeferredChildWireEvents(providerThreadIDs ...string) {
	for _, providerThreadID := range providerThreadIDs {
		for _, event := range s.takeDeferredChildWireEvents(providerThreadID) {
			if event.RequestID == "" {
				s.dispatchNotification(event.Method, event.Params)
				continue
			}
			requestID := json.Number(event.RequestID)
			s.dispatchServerRequest(event.Method, &requestID, event.Params, event.RawLine)
		}
	}
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
	providerThreadID := providerThreadIDFromParams(params)
	if !s.isUnmappedForeignProviderThread(providerThreadID) {
		s.handleServerRequest(method, id, params, line)
		return
	}

	requestID := ""
	if id != nil {
		requestID = id.String()
	}
	if s.deferChildWireEvent(providerThreadID, deferredChildWireEvent{
		Method:    method,
		Params:    params,
		RequestID: requestID,
		RawLine:   line,
	}) {
		return
	}
	s.warnChildRoutingOverflow(providerThreadID, method, id)
}
