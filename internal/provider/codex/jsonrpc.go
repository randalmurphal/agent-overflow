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
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			} `json:"error,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
		}
		if err := json.Unmarshal(resp, &rpcResp); err == nil {
			if rpcResp.Error != nil {
				return nil, &RPCError{
					Method:  method,
					Code:    rpcResp.Error.Code,
					Message: rpcResp.Error.Message,
					Data:    rpcResp.Error.Data,
				}
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

// RPCError is a JSON-RPC error response from the Codex app-server, kept
// typed so callers can branch on the code instead of matching the
// rendered message. The Error() text is unchanged from the untyped
// fmt.Errorf it replaced — IsNoActiveTurnRace still reads the message.
type RPCError struct {
	Method  string
	Code    int
	Message string
	// Data is the JSON-RPC error's optional `data` member, verbatim.
	//
	// Upstream attaches it to exactly the refusals it wants a client to
	// branch on rather than read — a `turn/steer` refused because the
	// running turn is a review or a compaction carries a serialized
	// TurnError here, `codexErrorInfo` and all
	// (request_processors/turn_processor.rs). Dropping it would leave those
	// indistinguishable from every other -32600 except by their English
	// message. Empty for the many refusals that carry none.
	Data json.RawMessage
}

func (e *RPCError) Error() string {
	if e == nil {
		return "codex: rpc error"
	}
	return fmt.Sprintf("codex: %s: %s (code %d)", e.Method, e.Message, e.Code)
}

// IsMethodUnsupported reports whether err is the app-server's answer to a
// method this binary does not implement — i.e. "upgrade codex", never
// "the request was wrong".
//
// Two shapes, both observed on real binaries:
//
//   - -32601 MethodNotFound, the JSON-RPC standard answer.
//   - -32600 InvalidRequest whose message carries serde's "unknown
//     variant" text naming the method. Codex deserializes the whole frame
//     into a ClientRequest enum, so an unrecognized method fails at
//     deserialization before any dispatch table is consulted. Verified
//     against codex-cli 0.146.0 (scratchpad spike, `thread/settings/updateX`
//     → -32600 "Invalid request: unknown variant `thread/settings/updateX`,
//     expected one of `initialize`, …").
//
// The method name is required rather than inferred from the error so a
// -32600 raised by a DIFFERENT unknown variant (a nested params enum, say)
// can never be read as "this method is missing".
func IsMethodUnsupported(err error, method string) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Code == -32601 {
		return true
	}
	return rpcErr.Code == -32600 &&
		strings.Contains(rpcErr.Message, "unknown variant `"+method+"`")
}

// IsThreadNotFound reports the authoritative thread/resume refusal for a
// provider cursor that no longer resolves. The JSON-RPC code alone is too broad
// (every invalid request uses it), so the provider adapter owns the explicit
// messages emitted by the current thread store and older app-server releases.
func IsThreadNotFound(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Method != "thread/resume" || rpcErr.Code != -32600 {
		return false
	}
	message := strings.TrimSpace(rpcErr.Message)
	return strings.HasPrefix(message, "no rollout found for thread id ") ||
		strings.HasPrefix(message, "thread not found:") || message == "thread not found"
}

// Thread writer-ownership refusals.
//
// Codex 0.147 extended its cross-process single-writer lock to legacy
// rollout histories: `LocalThreadStore::resume_thread` (and create) take a
// `WriterLockGuard` from
// codex-rs/thread-store/src/local/writer_lock.rs before opening a thread, and
// a lock another PROCESS holds fails the operation instead of waiting. The
// practical trigger is the user's own `codex` TUI sitting on the same thread.
//
// The refusal reaches the wire as a PLAIN INVALID-REQUEST — there is no
// `codexErrorInfo` kind to branch on, so this is deliberately a message match.
// `ThreadStoreError::Conflict` is folded together with `InvalidRequest` by
// `thread_store_mutation_error`
// (codex-rs/app-server/src/request_processors/thread_processor.rs), which maps
// both onto `invalid_request` / -32600. Upstream's own integration test pins
// the exact pair — `assert_eq!(error.error.code, -32600)` and
// `format!("thread {} already has an active writer", thread.id)` in
// codex-rs/app-server/tests/suite/v2/thread_resume.rs — so the substrings
// below are as stable as anything in this file's message-matching family
// (IsThreadNotFound, IsNoActiveTurnRace).
//
// Two distinct producers, one meaning for the user:
//
//   - "already has an active writer" — the cross-process FILE lock
//     (writer_lock.rs). Another codex process holds the thread.
//   - "already has a live local writer" — the in-process live-recorder map
//     (local/mod.rs `ensure_live_recorder_absent` / `insert_live_recorder`).
//     Same app-server already has the thread open.
var threadWriterConflictMarkers = []string{
	"already has an active writer",
	"already has a live local writer",
}

// ThreadWriterConflictMessage is the user-facing text for a thread another
// Codex process owns. It names the overwhelmingly likely culprit and the one
// action that resolves it, because there is nothing Agent Overflow can do from
// its side — the lock is advisory-by-process and only its holder can drop it.
const ThreadWriterConflictMessage = "This Codex thread is open in another Codex process " +
	"(most likely the terminal TUI). Close it there, then try again."

// ThreadWriterConflictError is a writer-ownership refusal, classified so the
// app layer can show ThreadWriterConflictMessage instead of a raw wire string.
// The original RPCError stays reachable through Unwrap for logs.
type ThreadWriterConflictError struct {
	// Wire is the app-server's own message, reachable through errors.As for
	// forensics. It names the thread id, which the user-facing text
	// deliberately does not — that asymmetry IS the field's purpose, so a
	// diagnosis can identify the locked thread without the toast doing so.
	// Nothing logs it today; it is the one piece of the refusal the
	// user-facing text destroys, and reconstructing it later is impossible.
	//
	// The refusing METHOD is deliberately not carried. It reached the struct
	// once and had no reader by construction: the lock is a property of the
	// THREAD, not of the method (see classifyThreadWriterConflict), so which
	// of the four locking RPCs happened to hit it says nothing a caller can
	// act on — and the wrapped RPCError still carries it for a log line.
	Wire string
	err  error
}

// Error is the same sentence for every conflict, nil receiver included: the
// message is a constant and nothing about this error is per-instance.
func (e *ThreadWriterConflictError) Error() string {
	return ThreadWriterConflictMessage
}

func (e *ThreadWriterConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// isThreadWriterConflict reports whether err is a writer-ownership refusal.
func isThreadWriterConflict(err error) bool {
	var conflict *ThreadWriterConflictError
	return errors.As(err, &conflict)
}

// classifyThreadWriterConflict upgrades a raw JSON-RPC refusal into
// ThreadWriterConflictError, or returns err unchanged.
//
// Applied at every RPC that can take a writer lock — thread/start,
// thread/resume (both the handshake and the mid-life reconcile), thread/fork,
// and the thread/read probe — rather than at one of them, because the lock is
// a property of the thread, not of the method, and a raw
// "thread <uuid> already has an active writer" reaching the user is the
// failure this exists to prevent.
func classifyThreadWriterConflict(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		return err
	}
	for _, marker := range threadWriterConflictMarkers {
		if strings.Contains(rpcErr.Message, marker) {
			return &ThreadWriterConflictError{Wire: rpcErr.Message, err: err}
		}
	}
	return err
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
// Steer itself already classifies the two current app-server messages
// ("no active turn to steer" and the expected-turn mismatch) onto the
// sentinel, so shape 1 covers them; the bare substring stays for an error
// that reached a caller without going through classifySteerRejection.
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
	// A turn that is running but refuses input is a different state and must
	// never read as a race — the fallback for a race is to open a new turn,
	// which is exactly the wrong answer during a review or a compaction.
	if IsTurnNotSteerable(err) {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "NoActiveTurn") ||
		isSteerTurnPreconditionMessage(message)
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

// IsAmbiguousTurnStartTimeout is the `turn/start` analog of
// IsAmbiguousSteerTimeout: the request was written to Codex but the
// JSON-RPC ack never arrived, so the turn — and its user-message echo —
// may already be running. Callers must not re-send the content on this
// error; they leave the pending echo confirmation in place instead.
func IsAmbiguousTurnStartTimeout(err error) bool {
	return IsRequestTimeout(err, "turn/start")
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

		// The abnormal-exit "error" (host did not initiate this close) plus
		// the unconditional "disconnected" — shared with claude so the two
		// read loops cannot drift on the signal triage gates its synthesized
		// turn-complete on. See provider.EmitTeardownStatus.
		provider.EmitTeardownStatus(s.emitEvent, s.threadID, s.proc, s.closing.Load())
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.emitEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("codex: read error: %v", err),
					Meta:      meta,
					Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
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
		s.dispatchServerRequest(msg.Method, msg.ID, msg.Params, line)
		return
	}

	// Notification: has method, no id.
	if msg.Method != "" {
		s.dispatchNotification(msg.Method, msg.Params)
		return
	}
}
