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
	deferredChildOwnershipTimeout   = 10 * time.Second
)

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
	rootThreadID := strings.TrimSpace(s.codexThreadID)
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
	if s.deferredChildWireEvents == nil {
		s.deferredChildWireEvents = make(map[string][]deferredChildWireEvent)
	}
	queuedForThread := s.deferredChildWireEvents[providerThreadID]
	if len(queuedForThread) >= maxDeferredChildEventsPerThread ||
		s.deferredChildWireCount >= maxDeferredChildEventsTotal ||
		eventBytes > maxDeferredChildEventBytes ||
		s.deferredChildWireBytes > maxDeferredChildEventBytes-eventBytes {
		return false
	}
	event.Params = append(json.RawMessage(nil), event.Params...)
	event.RawLine = append(json.RawMessage(nil), event.RawLine...)
	s.deferredChildWireEvents[providerThreadID] = append(queuedForThread, event)
	s.deferredChildWireCount++
	s.deferredChildWireBytes += eventBytes
	if s.deferredChildDeadlines == nil {
		s.deferredChildDeadlines = make(map[string]*time.Timer)
	}
	if s.deferredChildDeadlines[providerThreadID] == nil {
		threadID := providerThreadID
		s.deferredChildDeadlines[providerThreadID] = time.AfterFunc(deferredChildOwnershipTimeout, func() {
			s.expireDeferredChildWireEvents(threadID)
		})
	}
	return true
}

func (s *Session) takeDeferredChildWireEvents(providerThreadID string) []deferredChildWireEvent {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.deferredChildWireEvents[providerThreadID]
	if len(events) == 0 {
		return nil
	}
	delete(s.deferredChildWireEvents, providerThreadID)
	if timer := s.deferredChildDeadlines[providerThreadID]; timer != nil {
		timer.Stop()
		delete(s.deferredChildDeadlines, providerThreadID)
	}
	for _, event := range events {
		s.deferredChildWireBytes -= event.sizeBytes()
	}
	s.deferredChildWireCount -= len(events)
	if s.deferredChildWireCount < 0 {
		log.Printf("codex: deferred child routing event count underflow; resetting")
		s.deferredChildWireCount = 0
	}
	if s.deferredChildWireBytes < 0 {
		// Defensive only: every queue mutation is under mu, so reaching this
		// branch indicates an accounting bug rather than provider input.
		log.Printf("codex: deferred child routing byte count underflow; resetting")
		s.deferredChildWireBytes = 0
	}
	return events
}

func (s *Session) expireDeferredChildWireEvents(providerThreadID string) {
	events := s.takeDeferredChildWireEvents(providerThreadID)
	if len(events) == 0 || s.closing.Load() {
		return
	}
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
	warned := s.childRoutingWarned
	s.childRoutingWarned = true
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
	s.onEvent(provider.ProviderEvent{
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
