package codex

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	maxDeferredChildEventsPerThread = 128
	maxDeferredChildEventsTotal     = 512
	maxDeferredChildEventBytes      = 4 * 1024 * 1024
	maxDeferredChildThreadIDBytes   = 256
)

// deferredChildOwnershipTimeout is how long a thread's frames stay held before
// the routing deadline rejects them. Conn.startHoldTimer arms it; a test
// replaces that hook rather than this value.
const deferredChildOwnershipTimeout = 10 * time.Second

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

// retained returns e with its own copy of the line, for a frame kept past
// the read that produced it.
func (e deferredChildWireEvent) retained() deferredChildWireEvent {
	e.Params = append(json.RawMessage(nil), e.Params...)
	e.RawLine = append(json.RawMessage(nil), e.RawLine...)
	return e
}

func (e deferredChildWireEvent) sizeBytes() int {
	return len(e.Method) + len(e.Params) + len(e.RequestID) + len(e.RawLine)
}

// dispatchesWireThread reports whether this session dispatches a frame on
// providerThreadID itself: the root, any frame before the root is known, or a
// child the connection routes to this session, which it does once the child's
// spawn was emitted (registerChildRoute). The connection hands any other frame
// to the session the thread is routed to, or holds it until a spawn claims the
// thread or the deadline drops it, so a child's frame never reaches the root.
//
// A child frame this session dispatches therefore always has a spawn card:
// the route exists only for a thread whose ownership edge this session
// recorded (drainDeferredChildWireEvents), and no edge is deleted before
// Close, since close_agent keeps it. The attribution reads after this check
// (claimNotificationOwnership, handleServerRequest) rely on that, so an empty
// spawn card there means the frame is the root's.
func (s *Session) dispatchesWireThread(providerThreadID string, event deferredChildWireEvent, requestID *json.Number) bool {
	providerThreadID = strings.TrimSpace(providerThreadID)
	rootThreadID := strings.TrimSpace(s.rootThreadID())
	if providerThreadID == "" || rootThreadID == "" || providerThreadID == rootThreadID {
		return true
	}
	return s.conn.deliverOrHold(providerThreadID, s, event, requestID)
}

// drainDeferredChildWireEvents routes each live child this session owns to it
// and releases the frames the connection held for it. Every caller runs it
// after the child's spawn or resume row was emitted, which is what makes the
// route the signal that the child's frames may be delivered. A thread this
// session does not own, or a child close_agent closed, gets no route, so its
// frames stay held. Liveness is read and the route written under s.mu, so
// close_agent's teardown cannot interleave.
func (s *Session) drainDeferredChildWireEvents(providerThreadIDs ...string) {
	for _, providerThreadID := range providerThreadIDs {
		s.mu.Lock()
		moved := s.collab.childParentByThread[providerThreadID] != "" &&
			s.childLiveLocked(providerThreadID) &&
			s.conn.registerChildRoute(providerThreadID, s)
		s.mu.Unlock()
		if moved {
			s.flushEarlyFrames()
		}
	}
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
	requestID := ""
	if id != nil {
		requestID = id.String()
	}
	if !s.dispatchesWireThread(providerThreadIDFromParams(params), deferredChildWireEvent{
		Method:    method,
		Params:    params,
		RequestID: requestID,
		RawLine:   line,
	}, id) {
		return
	}
	s.handleServerRequest(method, id, params, line)
}
