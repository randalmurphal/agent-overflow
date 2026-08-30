package codex

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"agent-overflow/internal/provider"
)

// trackPendingApproval registers an interactive request. Uses the numeric
// JSON-RPC id rendered as a string so dedup (Bug B9) and response routing
// both use the same key — and so writeDrainResponse can parse it back into
// the id the JSON-RPC reply has to carry.
func (s *Session) trackPendingApproval(rpcID int64, resolveKind provider.EventKind, decisions ...*[]json.RawMessage) {
	s.trackPendingApprovalScoped(rpcID, resolveKind, s.rootThreadID(), decisions...)
}

func (s *Session) trackPendingApprovalScoped(rpcID int64, resolveKind provider.EventKind, scope string, decisions ...*[]json.RawMessage) {
	requestID := strconv.FormatInt(rpcID, 10)
	if resolveKind == provider.EventApprovalResolved && len(decisions) > 0 {
		s.approvals.TrackApprovalScoped(requestID, resolveKind, decisions[0], scope)
		return
	}
	s.approvals.TrackScoped(requestID, resolveKind, nil, scope)
}

// clearPendingApprovals is the Close-path drain. Latches the registry shut so
// late approvals are refused after teardown and emits resolved events
// with `decision: "lost"` (the session-died-mid-prompt signal triage
// uses to flip rows to errored).
func (s *Session) clearPendingApprovals() {
	s.drainPendingApprovals("lost", true, true)
}

// drainPendingApprovals resolves every outstanding approval and
// user-input request. For each one we:
//
//  1. Optionally write a JSON-RPC response (decline / error) to the provider so
//     the in-flight server request unblocks. Skipped silently when the
//     request ID is malformed (defensive only — trackPendingApproval
//     formats it from int64) or when writeResponse is false because the
//     provider process already exited.
//  2. Emit the matching EventApprovalResolved / EventUserInputResolved
//     so the frontend clears its prompt panel. User-input variants
//     additionally carry an empty `answers: {}` map to satisfy the
//     frontend type contract.
//
// closeSession=true is the Close path — latch the registry shut so late
// approval requests can't register as pending, and drop its dedup set
// since no duplicate response can reach the provider after Close.
// closeSession=false is the Interrupt path — the session keeps running
// and may receive new approval requests.
func (s *Session) drainPendingApprovals(decisionWord string, closeSession bool, writeResponse bool) {
	for _, released := range s.approvals.Drain(closeSession) {
		s.resolveDrainedApproval(released, decisionWord, writeResponse)
	}
}

func (s *Session) drainPendingApprovalsForScope(scope, decisionWord string, writeResponse bool) {
	for _, released := range s.approvals.DrainScope(scope) {
		s.resolveDrainedApproval(released, decisionWord, writeResponse)
	}
}

func (s *Session) resolveDrainedApproval(released provider.ResolvedApproval, decisionWord string, writeResponse bool) {
	if writeResponse {
		s.writeDrainResponse(released, decisionWord)
	}
	event := provider.ProviderEvent{
		Kind:      released.ResolveKind,
		ThreadID:  s.threadID,
		ItemID:    released.RequestID,
		Meta:      released.Meta(decisionWord),
		Timestamp: time.Now(),
	}
	if released.Scope != "" && released.Scope != s.rootThreadID() {
		event.ParentToolUseID = s.parentToolUseForProviderThread(released.Scope)
	}
	s.emitEvent(event)
}

// writeDrainResponse releases Codex from a pending server request by
// sending a JSON-RPC error with `data.reason = "turnTransition"`. This
// is the wire-correct way to abandon any kind of pending request
// (command-execution approval, file-change approval, permissions,
// user-input, MCP elicitation): Codex's app-server early-returns on
// this exact error shape via `is_turn_transition_server_request_error`
// (codex-rs/app-server/src/server_request_error.rs). Sending the
// success-shape decline instead works by accident — Codex falls
// through to a per-handler "client error" branch that logs noise and,
// for MCP elicitation specifically, picks `Decline` instead of the
// semantically-correct `Cancel`. Best-effort: a write failure during
// Close is logged, not surfaced.
func (s *Session) writeDrainResponse(released provider.ResolvedApproval, decisionWord string) {
	rpcID, err := strconv.ParseInt(released.RequestID, 10, 64)
	if err != nil {
		return
	}
	message := fmt.Sprintf("client request resolved because the turn state was changed (%s)", decisionWord)
	if err := s.writeErrorResponseWithData(rpcID, -1, message, map[string]any{
		"reason": codexTurnTransitionReason,
	}); err != nil {
		log.Printf("codex: drain response for %s (kind=%v): %v", released.RequestID, released.ResolveKind, err)
	}
}
