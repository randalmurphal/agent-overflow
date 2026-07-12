package codex

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"agent-overflow/internal/provider"
)

// pendingApproval tracks one in-flight interactive request so user
// responses, provider cancels, interrupt drains, and session close all
// resolve the same request ID exactly once.
type pendingApproval struct {
	resolveKind provider.EventKind
}

// trackPendingApproval registers an interactive request. Uses the numeric
// JSON-RPC id rendered as a string so dedup (Bug B9) and response routing
// both use the same key.
func (s *Session) trackPendingApproval(rpcID int64, resolveKind provider.EventKind) {
	requestID := fmt.Sprintf("%d", rpcID)
	s.approvalsMu.Lock()
	if s.approvalsClosed {
		s.approvalsMu.Unlock()
		return
	}
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]*pendingApproval)
	}
	s.pendingApprovals[requestID] = &pendingApproval{
		resolveKind: resolveKind,
	}
	// Starting a new pending request re-opens the ID: e.g. if we previously
	// resolved it and the provider re-sent the request (unusual, but
	// cheap to support).
	s.approvalDedup.Forget(requestID)
	s.approvalsMu.Unlock()
}

// claimApproval returns true when the caller is the first to answer the
// approval for requestID. False means either we already answered (Bug B9
// dedup) or the session is closing.
func (s *Session) claimApproval(requestID string, expectedKind provider.EventKind) bool {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	if s.approvalDedup.IsResolved(requestID) {
		return false
	}
	pending, hadPending := s.pendingApprovals[requestID]
	if !hadPending || pending.resolveKind != expectedKind {
		return false
	}
	delete(s.pendingApprovals, requestID)
	s.approvalDedup.MarkResolved(requestID)
	return true
}

// clearPendingApprovals is the Close-path drain. Sets approvalsClosed so
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
// closeSession=true is the Close path — set approvalsClosed so late
// approval requests can't register as pending, and drop the
// resolvedApprovals dedup set since no duplicate response can reach
// the provider after Close. closeSession=false is the Interrupt path —
// the session keeps running and may receive new approval requests.
func (s *Session) drainPendingApprovals(decisionWord string, closeSession bool, writeResponse bool) {
	s.approvalsMu.Lock()
	if closeSession {
		s.approvalsClosed = true
	}
	pending := s.pendingApprovals
	s.pendingApprovals = nil
	if closeSession {
		s.approvalDedup.Reset()
	}
	s.approvalsMu.Unlock()

	for requestID, p := range pending {
		if writeResponse {
			s.writeDrainResponse(requestID, p, decisionWord)
		}

		metaFields := map[string]any{
			"requestId": requestID,
			"decision":  decisionWord,
		}
		if p.resolveKind == provider.EventUserInputResolved {
			metaFields["answers"] = map[string]any{}
		}
		meta, _ := json.Marshal(metaFields)
		s.emitEvent(provider.ProviderEvent{
			Kind:      p.resolveKind,
			ThreadID:  s.threadID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}
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
func (s *Session) writeDrainResponse(requestID string, p *pendingApproval, decisionWord string) {
	rpcID, err := strconv.ParseInt(requestID, 10, 64)
	if err != nil {
		return
	}
	message := fmt.Sprintf("client request resolved because the turn state was changed (%s)", decisionWord)
	if err := s.writeErrorResponseWithData(rpcID, -1, message, map[string]any{
		"reason": codexTurnTransitionReason,
	}); err != nil {
		log.Printf("codex: drain response for %s (kind=%v): %v", requestID, p.resolveKind, err)
	}
}
