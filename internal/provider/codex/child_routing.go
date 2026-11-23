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

// dispatchesWireThread reports whether this session dispatches a frame on
// providerThreadID itself: the root, any frame before the root is known, or a
// child whose spawn ownership is recorded. Any other frame is handed to the
// connection, which holds it until a spawn claims the thread
// (drainDeferredChildWireEvents) or rejects and drops it at the deadline, so
// a child's frame never reaches the root. held reports whether the connection
// accepted the frame rather than rejecting it for its bounds.
func (s *Session) dispatchesWireThread(providerThreadID string, event deferredChildWireEvent, requestID *json.Number) (dispatch, held bool) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	rootThreadID := strings.TrimSpace(s.rootThreadID())
	if providerThreadID == "" || rootThreadID == "" || providerThreadID == rootThreadID ||
		s.parentToolUseForProviderThread(providerThreadID) != "" {
		return true, false
	}
	return false, s.handChildFrame(providerThreadID, event, requestID)
}

// handChildFrame gives the connection a frame for a thread this session did
// not own when it looked, and reports whether the connection holds it. A
// spawn recorded after that look may already have drained the thread's held
// frames, before this one joined them; claiming again releases it.
func (s *Session) handChildFrame(providerThreadID string, event deferredChildWireEvent, requestID *json.Number) bool {
	held := s.conn.holdChildFrame(providerThreadID, event, requestID)
	if held && s.parentToolUseForProviderThread(providerThreadID) != "" {
		s.drainDeferredChildWireEvents(providerThreadID)
	}
	return held
}

// drainDeferredChildWireEvents routes each newly owned child thread to this
// session and releases the frames the connection held for it.
func (s *Session) drainDeferredChildWireEvents(providerThreadIDs ...string) {
	for _, providerThreadID := range providerThreadIDs {
		s.mu.Lock()
		delete(s.childRouting.recoveryPending, providerThreadID)
		s.mu.Unlock()
		if s.conn.registerChildRoute(providerThreadID, s) {
			s.flushEarlyFrames()
		}
	}
}

// forgetUnclaimedChild drops what this session kept for a thread the
// connection stopped holding at its deadline: the pending ownership recovery
// and the display metadata of a thread no spawn claimed.
func (s *Session) forgetUnclaimedChild(providerThreadID string) {
	s.mu.Lock()
	delete(s.childRouting.recoveryPending, providerThreadID)
	s.mu.Unlock()
	s.deleteUnownedAgentMeta(providerThreadID)
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
	if dispatch, _ := s.dispatchesWireThread(providerThreadIDFromParams(params), deferredChildWireEvent{
		Method:    method,
		Params:    params,
		RequestID: requestID,
		RawLine:   line,
	}, id); !dispatch {
		return
	}
	s.handleServerRequest(method, id, params, line)
}
