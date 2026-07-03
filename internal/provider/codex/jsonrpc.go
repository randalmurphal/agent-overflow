package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// sendRequest sends a JSON-RPC request and waits for a response. On
// timeout or context cancellation we remove the pending entry
// atomically with the lock, then drop the buffered response (if any)
// so the channel does not leak a record that no one will read. A
// response that arrives AFTER we delete the pending entry is dropped
// by dispatchLine's default branch — we cannot leak a late response
// once the pending entry is gone.
func (s *Session) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)

	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	// abandon removes the pending entry under the lock and drains the
	// buffered response (if any) so a late write from dispatchLine
	// lands in a channel nobody is holding. Called exactly once by
	// whichever branch of the select below runs — we don't use a
	// defer so the drain happens BEFORE we return rather than after,
	// which eliminates the window where a late response can land in
	// the buffer-1 channel unobserved.
	abandon := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		select {
		case <-ch:
		default:
		}
	}

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}

	data, err := json.Marshal(msg)
	if err != nil {
		abandon()
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}

	if err := s.proc.WriteLine(data); err != nil {
		abandon()
		return nil, err
	}

	select {
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		// The happy path also needs to clear the pending entry. We do
		// it here rather than via defer so abandon's lock pattern is
		// the single source of truth.
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("codex: %s: session stopped before request completed", method)
		}
		var rpcResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
		}
		if err := json.Unmarshal(resp, &rpcResp); err == nil {
			if rpcResp.Error != nil {
				return nil, fmt.Errorf("codex: %s: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
			}
			if len(rpcResp.Result) > 0 {
				return rpcResp.Result, nil
			}
		}
		return resp, nil
	case <-time.After(s.requestTimeout()):
		abandon()
		return nil, &RequestTimeoutError{Method: method}
	}
}

// RequestTimeoutError means a JSON-RPC request was written to Codex but no
// response arrived before the client-side timeout. Callers should treat this
// as an ambiguous ACK-missing state, not proof that Codex rejected the request.
type RequestTimeoutError struct {
	Method string
}

func (e *RequestTimeoutError) Error() string {
	if e == nil {
		return "codex: request timeout"
	}
	return fmt.Sprintf("codex: %s: timeout", e.Method)
}

// IsRequestTimeout reports whether err wraps a Codex JSON-RPC timeout for the
// given method. Pass an empty method to match any Codex request timeout.
func IsRequestTimeout(err error, method string) bool {
	var timeoutErr *RequestTimeoutError
	if !errors.As(err, &timeoutErr) {
		return false
	}
	return method == "" || timeoutErr.Method == method
}

// IsNoActiveTurnRace reports whether err is one of the two
// no-active-turn race shapes the App's flush-dispatch path falls back
// on. The two shapes:
//
//  1. ErrNoActiveTurn — the typed sentinel Steer returns when the
//     in-process state says no turn is active.
//  2. A wire-level "NoActiveTurn" carried by an opaque JSON-RPC error
//     message. The substring is stable per upstream
//     `codex-rs/core/src/session/mod.rs`; we don't introspect the
//     typed JSON-RPC error code because the upstream code path emits
//     a generic InvalidRequest with the discriminating substring in
//     the message.
//
// Callers (App's flush-queue dispatch) react to a positive result by
// retrying via Send instead of Steer.
func IsNoActiveTurnRace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoActiveTurn) {
		return true
	}
	return strings.Contains(err.Error(), "NoActiveTurn")
}

// IsAmbiguousSteerTimeout reports whether err is the ambiguous
// `turn/steer` JSON-RPC timeout where the App cannot decide between
// "Codex never accepted the steer" and "Codex accepted but the
// response never landed." Callers (App's flush-queue dispatch) react
// by surfacing the queued message back to the composer rather than
// double-sending.
func IsAmbiguousSteerTimeout(err error) bool {
	return IsRequestTimeout(err, "turn/steer")
}

// defaultRequestTimeout bounds how long sendRequest waits for a JSON-RPC
// response. Overridable by tests via Session.requestTimeoutOverride.
const defaultRequestTimeout = 30 * time.Second

// requestTimeout returns the active JSON-RPC response timeout. Tests set
// a much shorter value via the unexported requestTimeoutOverride field
// so they can exercise the timeout + late-response path without waiting
// 30 seconds.
func (s *Session) requestTimeout() time.Duration {
	if s.requestTimeoutOverride > 0 {
		return s.requestTimeoutOverride
	}
	return defaultRequestTimeout
}

// writeNotification sends a JSON-RPC notification (no id, no response expected).
func (s *Session) writeNotification(method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal notification: %w", err)
	}
	return s.proc.WriteLine(data)
}

// writeErrorResponse sends a JSON-RPC error response with the given code and message.
func (s *Session) writeErrorResponse(id int64, code int, message string) error {
	return s.writeErrorResponseWithData(id, code, message, nil)
}

// writeErrorResponseWithData sends a JSON-RPC error response with an
// optional `data` payload. Codex's app-server inspects `data.reason`
// to decide how to handle in-flight server requests when the turn is
// transitioning — see `is_turn_transition_server_request_error` at
// codex-rs/app-server/src/server_request_error.rs and the early-return
// branches in bespoke_event_handling.rs (line 2390 for patch approval,
// 2447 for exec approval, 2494 for user input, 2603 for MCP
// elicitation, 2680 for permissions).
func (s *Session) writeErrorResponseWithData(id int64, code int, message string, data any) error {
	errFields := map[string]any{
		"code":    code,
		"message": message,
	}
	if data != nil {
		errFields["data"] = data
	}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   errFields,
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal error response: %w", err)
	}
	return s.proc.WriteLine(encoded)
}

// codexTurnTransitionReason is the magic string Codex's app-server
// uses to recognize a JSON-RPC error as "the turn already ended,
// drop this request silently". See
// codex-rs/app-server/src/server_request_error.rs's
// `TURN_TRANSITION_PENDING_REQUEST_ERROR_REASON` constant. Sending
// this in the error `data.reason` makes drain-on-interrupt and
// drain-on-close clean up without falling through to "request failed
// with client error" log noise on Codex's side, AND for MCP
// elicitations, it specifically maps the response to
// `McpServerElicitationAction::Cancel` (the right semantics for
// "user aborted") rather than `Decline` (which means "user said no
// to this specific action").
const codexTurnTransitionReason = "turnTransition"

// writeResponse sends a JSON-RPC response (to server requests like approvals).
func (s *Session) writeResponse(id int64, result any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal response: %w", err)
	}
	return s.proc.WriteLine(data)
}

// readLoop reads stdout and dispatches JSON-RPC messages.
func (s *Session) readLoop() {
	defer func() {
		if s.readDone != nil {
			defer close(s.readDone)
		}

		// Unblock all pending requests.
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()

		// If the app-server exits while an approval or user-input request is
		// waiting, resolve it as lost so the frontend prompt does not linger.
		// The process is already gone, so only emit local resolution events.
		s.drainPendingApprovals("lost", true, false)

		if !s.closing.Load() {
			// Any unexpected read-loop exit while we weren't the one
			// closing is abnormal — including a clean exit-code-0
			// without a host-initiated close. Triage gates synthesizing
			// the truncated turn-complete on the "error" signal, so a
			// missed emission here leaves the FE working indicator
			// stuck. MarshalProcessExitMeta tolerates a nil exitErr.
			exitErr := provider.WaitProcessExitErr(s.proc)
			s.onEvent(provider.ProviderEvent{
				Kind:      provider.EventSessionStatus,
				ThreadID:  s.threadID,
				Content:   "error",
				Meta:      provider.MarshalProcessExitMeta(exitErr, s.proc.StderrTail()),
				Timestamp: time.Now(),
			})
		}

		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("codex: read error: %v", err),
					Meta:      meta,
					Timestamp: time.Now(),
				})
			}
			return
		}

		s.dispatchLine(line)
	}
}

// dispatchLine classifies a JSON-RPC line and routes it.
func (s *Session) dispatchLine(line []byte) {
	var msg struct {
		ID     *json.Number    `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("codex: invalid JSON line: %v", err)
		return
	}

	// Response: has id, no method.
	if msg.ID != nil && msg.Method == "" {
		id, err := msg.ID.Int64()
		if err != nil {
			log.Printf("codex: response has non-integer ID %q: %v", msg.ID.String(), err)
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[id]
		s.mu.Unlock()
		if ok {
			// Non-blocking send: if the request already timed out and the channel
			// was removed or is full, drop the response silently.
			select {
			case ch <- line:
			default:
			}
		}
		return
	}

	// Server request: has both id and method (approval flow).
	if msg.ID != nil && msg.Method != "" {
		s.handleServerRequest(msg.Method, msg.ID, msg.Params, line)
		return
	}

	// Notification: has method, no id.
	if msg.Method != "" {
		s.dispatchNotification(msg.Method, msg.Params)
		return
	}
}
