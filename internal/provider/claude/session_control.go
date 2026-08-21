package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// controlResponsePrefix matches the CLI's reply to our outbound
// control_requests (stop_task, set_permission_mode). Prefix-gating
// it the same way as controlRequestPrefix keeps streaming deltas off
// the secondary json.Unmarshal path.
var controlResponsePrefix = []byte(`{"type":"control_response"`)

// DefaultControlRequestTimeout bounds how long outbound Claude control
// requests wait for the CLI's control_response before returning a timeout
// error. The verified stop_task spike observed sub-100ms round-trips on
// Claude CLI 2.1.112; ten seconds is a generous ceiling that still fails
// loudly if the CLI is wedged.
const DefaultControlRequestTimeout = 10 * time.Second

// controlResponseResult carries the outcome of an outbound control_request
// round-trip from the read loop back to the waiting caller. Exactly one of
// errMsg or ok is set: ok=true on subtype=success, errMsg populated on
// subtype=error. A nil pointer means the session closed before the response
// arrived. payload carries the inner `response.response` object — set for
// subtype=success when the request shape returns structured data (e.g.
// mcp_authenticate's {authUrl, requiresUserAction}); empty otherwise.
type controlResponseResult struct {
	ok      bool
	errMsg  string
	payload json.RawMessage
}

// pendingControlRequest is the pendingControlRequests map value: the
// waiter's channel plus whether this round-trip is an `interrupt`
// (derived from the request's wire subtype in sendControlRequest — only
// Session.Interrupt sends it). The read loop uses the flag to call
// Parser.MarkInterruptAcked on a successful ack.
type pendingControlRequest struct {
	ch          chan *controlResponseResult
	isInterrupt bool
}

// normalizeClaudePermissionMode keeps only permission modes the CLI accepts
// on `--permission-mode` / `set_permission_mode`. Anything else collapses to
// "default" (supervised prompting), so every mode AO can select must be
// listed here — omitting one would silently widen a restricted session into
// the prompting default, which for an unattended run means a hang rather
// than a refusal.
func normalizeClaudePermissionMode(mode string) string {
	switch mode {
	case "acceptEdits", "auto", "bypassPermissions", "plan", "dontAsk":
		return mode
	default:
		return "default"
	}
}

func (s *Session) desiredPermissionModeForTurn(mode provider.InteractionMode) string {
	if provider.NormalizeInteractionMode(string(mode)) == provider.ModePlan {
		return "plan"
	}
	s.permissionModeMu.RLock()
	defer s.permissionModeMu.RUnlock()
	return s.basePermissionMode
}

func (s *Session) getCurrentPermissionMode() string {
	s.permissionModeMu.RLock()
	defer s.permissionModeMu.RUnlock()
	return normalizeClaudePermissionMode(s.currentPermissionMode)
}

func (s *Session) setCurrentPermissionMode(mode string) {
	s.permissionModeMu.Lock()
	s.currentPermissionMode = normalizeClaudePermissionMode(mode)
	s.permissionModeMu.Unlock()
}

func (s *Session) setPermissionMode(ctx context.Context, mode string) error {
	mode = normalizeClaudePermissionMode(mode)
	if mode == s.getCurrentPermissionMode() {
		return nil
	}
	opName := "set permission mode " + mode
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "set_permission_mode",
		"mode":    mode,
	})
	if err != nil {
		return err
	}
	if err := interpretControlResponse(res, opName); err != nil {
		return err
	}
	s.setCurrentPermissionMode(mode)
	return nil
}

// SetInteractionMode applies a chat/plan mode change to the live Claude
// permission mode. The next Send also sets this defensively, but exposing the
// operation lets the app reflect a user toggling Plan Mode while the session is
// already running.
func (s *Session) SetInteractionMode(ctx context.Context, mode provider.InteractionMode) error {
	normalized := provider.NormalizeInteractionMode(string(mode))
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(normalized)); err != nil {
		return err
	}
	s.interactionMode = normalized
	return nil
}

// sendControlRequest is the shared round-trip for every outbound
// control_request the session originates (interrupt, stop_task,
// set_permission_mode). It allocates a request_id, registers the
// pending response channel, marshals + writes the envelope, and
// blocks on either ctx.Done, the configured timeout, or the matching
// control_response. Errors are wrapped with "claude: <opName>: ..."
// so callers don't repeat the prefix; the raw result is returned for
// the caller to interpret (success vs error subtype) — usually via
// interpretControlResponse, except where the caller has additional
// per-success side effects (e.g. setPermissionMode caching the new
// mode).
func (s *Session) sendControlRequest(ctx context.Context, opName string, request map[string]any) (*controlResponseResult, error) {
	requestID := s.allocateControlRequestID()
	ch := make(chan *controlResponseResult, 1)
	isInterrupt := request["subtype"] == "interrupt"
	if !s.registerControlRequest(requestID, pendingControlRequest{ch: ch, isInterrupt: isInterrupt}) {
		return nil, fmt.Errorf("claude: %s: session closing", opName)
	}

	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: marshal %s: %w", opName, err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: write %s: %w", opName, err)
	}

	timeout := s.controlRequestTimeout
	if timeout <= 0 {
		timeout = DefaultControlRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: %w", opName, ctx.Err())
	case <-timer.C:
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: timeout after %s", opName, timeout)
	case res, ok := <-ch:
		// deliverControlResponse already removed the entry under lock;
		// nothing for us to release here.
		if !ok || res == nil {
			return nil, fmt.Errorf("claude: %s: session closed before response", opName)
		}
		return res, nil
	}
}

// interpretControlResponse converts a delivered control_response into
// the standard success-or-wrapped-error pair every caller needs. Used
// directly by Interrupt and StopTask; setPermissionMode inlines the
// equivalent logic because it has a per-success side effect to run.
func interpretControlResponse(res *controlResponseResult, opName string) error {
	if res.ok {
		return nil
	}
	if res.errMsg == "" {
		return fmt.Errorf("claude: %s: provider returned unspecified error", opName)
	}
	return fmt.Errorf("claude: %s: %s", opName, res.errMsg)
}

// allocateControlRequestID generates a request_id unique within the
// session. Format is a short "so-<n>" prefix so logs and wire samples
// make it clear the id originated here.
func (s *Session) allocateControlRequestID() string {
	s.controlRequestMu.Lock()
	s.controlRequestSeq++
	seq := s.controlRequestSeq
	s.controlRequestMu.Unlock()
	return fmt.Sprintf("so-%d", seq)
}

// registerControlRequest stores the pending channel under the request_id.
// Returns false when Close has run (the closing flag flipped and the
// pending map has been drained) so late control callers fail fast
// instead of parking on a channel nobody will deliver to.
//
// The closing check happens UNDER controlRequestMu so the clearPendingControlRequests
// / registerControlRequest pair serialises correctly: if Close wins the
// lock first, the registration fails; if a concurrent control request wins
// it first, the entry is visible to the subsequent clearPendingControlRequests
// drain. Without this ordering, a late registration could leak a
// pending entry past Close.
func (s *Session) registerControlRequest(requestID string, pending pendingControlRequest) bool {
	s.controlRequestMu.Lock()
	defer s.controlRequestMu.Unlock()
	if s.closing.Load() {
		return false
	}
	if s.pendingControlRequests == nil {
		s.pendingControlRequests = make(map[string]pendingControlRequest)
	}
	s.pendingControlRequests[requestID] = pending
	return true
}

// releaseControlRequest removes the pending entry and drains the channel so
// a late read-loop delivery lands in a discarded buffer. Called from
// timeout / cancel / error branches so the map never leaks entries and
// the single-slot channel never blocks a reader that already gave up.
func (s *Session) releaseControlRequest(requestID string) {
	s.controlRequestMu.Lock()
	pending, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-pending.ch:
	default:
	}
}

// deliverControlResponse is the read-loop-side half: it matches an
// inbound control_response to a pending outbound control_request and delivers the
// result. Unknown request_ids are returned as delivered=false so the
// caller can log once and drop. wasInterrupt reports whether the
// matched request was Session.Interrupt's — the read loop uses it to
// flag the parser before the CLI's result line is parsed.
func (s *Session) deliverControlResponse(requestID string, res *controlResponseResult) (wasInterrupt, delivered bool) {
	s.controlRequestMu.Lock()
	pending, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
	if !ok {
		return false, false
	}
	select {
	case pending.ch <- res:
	default:
		// Channel already drained by timeout — nothing to do.
	}
	return pending.isInterrupt, true
}

// clearPendingControlRequests closes every outstanding control-request waiter so
// Close doesn't strand a caller. Mirrors clearPendingApprovals.
func (s *Session) clearPendingControlRequests() {
	s.controlRequestMu.Lock()
	pending := s.pendingControlRequests
	s.pendingControlRequests = nil
	s.controlRequestMu.Unlock()
	for _, p := range pending {
		// A nil send signals "session closing" — the caller returns a
		// clean error rather than hanging forever waiting on a
		// control_response the dead subprocess will never emit.
		select {
		case p.ch <- nil:
		default:
		}
	}
}

// handleControlResponseLine decodes a `control_response` NDJSON line
// and routes it to the waiting control-request caller by request_id. Called
// only from the read loop's prefix-gated branch, so all the work
// happens off the streaming hot path.
//
// Unknown request_ids are logged and dropped — the CLI might emit a
// duplicate or late reply after the session has already released its
// pending entry; silently discarding it keeps the read loop alive
// while still leaving a breadcrumb. These lines are rare in practice
// (one per out-of-band reply) so the log isn't rate-limited. Malformed
// JSON is likewise logged (not fatal): a garbled control_response
// shouldn't take the subprocess down.
func (s *Session) handleControlResponseLine(line []byte) {
	var raw struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Error     string          `json:"error"`
			Response  json.RawMessage `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		log.Printf("claude: malformed control_response line: %v", err)
		return
	}
	if raw.Type != "control_response" {
		// Prefix-only false positive (e.g. an unrelated envelope whose
		// serialized bytes happened to start with `{"type":"control_response`
		// — shouldn't happen in practice, but the check is cheap).
		return
	}

	requestID := raw.Response.RequestID
	if requestID == "" {
		log.Printf("claude: control_response missing request_id: %s", string(line[:min(len(line), 200)]))
		return
	}

	res := &controlResponseResult{}
	switch raw.Response.Subtype {
	case "success":
		res.ok = true
		res.payload = raw.Response.Response
	case "error":
		res.errMsg = raw.Response.Error
	default:
		// The CLI only emits success / error per the wire reference;
		// unknown subtypes get recorded as errors so the waiting caller
		// surfaces a clear message rather than silently hanging.
		res.errMsg = fmt.Sprintf("unexpected control_response subtype %q", raw.Response.Subtype)
	}

	wasInterrupt, delivered := s.deliverControlResponse(requestID, res)
	if !delivered {
		log.Printf("claude: control_response with no pending request_id %q (subtype=%s)", requestID, raw.Response.Subtype)
		return
	}
	// A successful interrupt ack flags the parser so the upcoming result
	// envelope classifies as user-aborted even when its `errors[]`
	// wording has no "aborted"/"interrupted" marker (claude 2.1.170).
	// Safe to set here without locking: this function runs on the read
	// loop, the same goroutine as ParseLine, and the CLI writes the ack
	// before the result line (verified 6/6 on 2.1.170 — see
	// claude-wire.md §result). A timed-out interrupt whose ack arrives
	// after releaseControlRequest is NOT delivered and never marks — the
	// classification then falls back to the errors[] heuristic.
	if wasInterrupt && res.ok {
		s.parser.MarkInterruptAcked()
	}
}
