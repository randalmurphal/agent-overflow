package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// controlRequestPrefix is the first bytes of a Claude control_request
// NDJSON line. Gating the ExitPlanMode handler on this prefix saves a
// json.Unmarshal on every assistant/user/stream_event line (which is
// the hot path during streaming). False positives are benign — the
// subsequent Request.Subtype / ToolName check still filters.
var controlRequestPrefix = []byte(`{"type":"control_request"`)

// controlCancelRequestPrefix matches the CLI's "abandon this prior
// control_request" notification. The CLI emits this for any in-flight
// can_use_tool callback that an interrupt aborts (the SDK fires the
// AbortSignal on the pending callback; the CLI side wires that to a
// control_cancel_request on stdout — see Python SDK
// _internal/query.py:272-278). We must NOT write a control_response
// for these — the CLI has already given up on the request — so the
// prefix gate routes them to a separate cleanup-only handler.
var controlCancelRequestPrefix = []byte(`{"type":"control_cancel_request"`)

type controlRequestEnvelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	} `json:"request"`
}

// pendingApproval tracks a single in-flight interactive request so user
// responses, provider cancels, and session close all resolve the same
// request ID exactly once.
type pendingApproval struct {
	resolveKind        provider.EventKind
	userInputQuestions []provider.UserInputQuestion
}

// RespondToApproval sends a tool-use approval decision back to the CLI.
// Accepts both Codex-native values (accept, acceptForSession, decline, cancel)
// and legacy values (allow, allow_session, deny) for backward compatibility.
// When resp.UpdatedInput or resp.UpdatedPermissions are non-empty and the
// decision is an allow, the raw JSON is forwarded to the CLI as the
// Claude-SDK-compatible CanUseTool response fields.
//
// Responding twice for the same RequestID returns ErrApprovalAlreadyResolved
// (Bug B9).
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	if !s.claimApproval(resp.RequestID, provider.EventApprovalResolved) {
		return ErrApprovalAlreadyResolved
	}
	data, err := buildApprovalResponse(resp)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	decision, err := provider.NormalizeUserInputDecision(resp.Decision)
	if err != nil {
		return err
	}
	questions := s.pendingUserInputQuestions(resp.RequestID)
	if !s.claimApproval(resp.RequestID, provider.EventUserInputResolved) {
		return ErrApprovalAlreadyResolved
	}
	approval := provider.ApprovalResponse{
		RequestID: resp.RequestID,
		Decision:  decision,
	}
	inputFields := map[string]any{
		"answers": claudeAskUserQuestionAnswers(questions, resp.Answers),
	}
	if len(questions) > 0 {
		inputFields["questions"] = questions
	}
	input, err := json.Marshal(inputFields)
	if err != nil {
		return fmt.Errorf("claude: marshal user input answers: %w", err)
	}
	approval.UpdatedInput = input
	data, err := buildApprovalResponse(approval)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

// claudeAskUserQuestionAnswers projects the user's selections into the shape
// Claude Code's AskUserQuestion tool consumes: question key -> answer string,
// with multi-select answers comma-joined.
//
// The comma-join is Claude Code's contract, not a lossy shortcut we picked
// (verified against the installed CLI's embedded schema, 2.1.168): the tool's
// result schema is `answers: record(string, string)` and documents
// "multi-select answers are comma-separated", so the model only ever sees a
// joined string. The injection point we actually write to (the permission
// component's updatedInput.answers) accepts an array too, but preprocesses it
// by joining with the identical ", " before validating as a string -- so
// sending map[string][]string here is accepted-but-equivalent, a no-op rather
// than a fix. There is no lossless multi-select form at the model layer; do
// not "fix" this into an array.
//
// Structured fidelity for display/history is preserved on a separate path that
// never reaches the model: triage's mergeUserInputAnswersIntoLaunch persists
// the raw per-question arrays onto item.meta.answers, which the AskUserQuestion
// history card prefers over this joined echo. That path keeps comma-containing
// labels and custom free-text intact for the UI. This function feeds the model
// only.
func claudeAskUserQuestionAnswers(questions []provider.UserInputQuestion, answers map[string]provider.UserInputAnswer) map[string]string {
	out := make(map[string]string, len(answers))
	used := make(map[string]struct{}, len(answers))
	keyCounts := claudeAskUserQuestionKeyCounts(questions)
	for _, question := range questions {
		answer, sourceKey, ok := answerForClaudeQuestion(question, answers)
		if !ok {
			continue
		}
		key := claudeAskUserQuestionAnswerKey(question, sourceKey, keyCounts)
		out[key] = strings.Join([]string(answer), ", ")
		used[sourceKey] = struct{}{}
	}
	for key, answer := range answers {
		if _, ok := used[key]; ok {
			continue
		}
		out[key] = strings.Join([]string(answer), ", ")
	}
	return out
}

// AskUserQuestionAnswers projects the user's selections into the
// question-key→answer map Claude Code's AskUserQuestion tool consumes
// (multi-select comma-joined). It is the exported seam for the interactive
// (TUI) provider's hook answer-back, which feeds the same projection into
// hookSpecificOutput.updatedInput rather than a stdin control_response — so
// both Claude transports share one copy of the contract rather than drifting.
// See claudeAskUserQuestionAnswers for why the comma-join is the contract.
func AskUserQuestionAnswers(questions []provider.UserInputQuestion, answers map[string]provider.UserInputAnswer) map[string]string {
	return claudeAskUserQuestionAnswers(questions, answers)
}

func claudeAskUserQuestionKeyCounts(questions []provider.UserInputQuestion) map[string]int {
	counts := make(map[string]int, len(questions)*3)
	for _, question := range questions {
		for _, key := range []string{question.Question, question.Header, question.ID} {
			key = strings.TrimSpace(key)
			if key != "" {
				counts[key]++
			}
		}
	}
	return counts
}

func claudeAskUserQuestionAnswerKey(question provider.UserInputQuestion, sourceKey string, keyCounts map[string]int) string {
	for _, key := range []string{question.Question, question.Header, question.ID} {
		key = strings.TrimSpace(key)
		if key != "" && keyCounts[key] == 1 {
			return key
		}
	}
	if sourceKey != "" {
		return sourceKey
	}
	return strings.TrimSpace(question.ID)
}

func answerForClaudeQuestion(question provider.UserInputQuestion, answers map[string]provider.UserInputAnswer) (provider.UserInputAnswer, string, bool) {
	for _, key := range []string{question.ID, question.Header, question.Question} {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if answer, ok := answers[key]; ok {
			return answer, key, true
		}
	}
	return nil, "", false
}

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered so callers can surface a clear
// message instead of silently shadowing the previous decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("claude: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// trackPendingApproval registers a pending interactive request.
func (s *Session) trackPendingApproval(requestID string, resolveKind provider.EventKind) {
	s.trackPendingApprovalWithQuestions(requestID, resolveKind, nil)
}

func (s *Session) trackPendingApprovalWithQuestions(requestID string, resolveKind provider.EventKind, questions []provider.UserInputQuestion) {
	if requestID == "" {
		return
	}
	s.approvalsMu.Lock()
	if s.approvalsClosed {
		s.approvalsMu.Unlock()
		return
	}
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]*pendingApproval)
	}
	s.pendingApprovals[requestID] = &pendingApproval{
		resolveKind:        resolveKind,
		userInputQuestions: append([]provider.UserInputQuestion(nil), questions...),
	}
	// Starting a new pending request re-opens the ID in case the provider
	// re-sent the request after a response.
	s.approvalDedup.Forget(requestID)
	s.approvalsMu.Unlock()
}

func (s *Session) pendingUserInputQuestions(requestID string) []provider.UserInputQuestion {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	pending := s.pendingApprovals[requestID]
	if pending == nil || len(pending.userInputQuestions) == 0 {
		return nil
	}
	return append([]provider.UserInputQuestion(nil), pending.userInputQuestions...)
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

// clearPendingApprovals resolves every outstanding interactive request
// with a "lost" decision. It also drops the dedup set: once Close has
// been called, no duplicate response can land at the provider, so the
// memory cost of keeping the IDs around is pure overhead.
func (s *Session) clearPendingApprovals() {
	s.approvalsMu.Lock()
	s.approvalsClosed = true
	pending := s.pendingApprovals
	s.pendingApprovals = nil
	s.approvalDedup.Reset()
	s.approvalsMu.Unlock()
	for requestID, p := range pending {
		// Decision "lost" signals session-ended-mid-prompt to triage
		// (internal/triage/approvals.go:198 maps it to status=errored
		// in the store). Different from the user-driven "cancel" the
		// control_cancel_request handler emits — that one means the
		// CLI itself abandoned the request after an interrupt; this
		// one means the session is going away.
		metaFields := map[string]any{
			"requestId": requestID,
			"decision":  "lost",
		}
		if p.resolveKind == provider.EventUserInputResolved {
			// Frontend expects answers on UserInputResolved events; empty
			// map keeps the type contract clean even when no user reply
			// was ever submitted.
			metaFields["answers"] = map[string]any{}
		}
		meta, _ := json.Marshal(metaFields)
		s.onEvent(provider.ProviderEvent{
			Kind:      p.resolveKind,
			ThreadID:  s.threadID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}
}

func (s *Session) handleControlRequest(raw controlRequestEnvelope) (handled bool, fatalMessage string, err error) {
	handled, err = s.handleExitPlanModeRequest(raw)
	if err != nil || handled {
		if err != nil && handled {
			return handled, "claude: exit plan mode response failed", err
		}
		return handled, "", err
	}
	handled, err = s.handleFullAccessToolRequest(raw)
	if err != nil && handled {
		return handled, "claude: full-access approval response failed", err
	}
	return handled, "", err
}

func (s *Session) handleFullAccessToolRequest(raw controlRequestEnvelope) (bool, error) {
	if s.getCurrentPermissionMode() != "bypassPermissions" {
		return false, nil
	}
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" {
		return false, nil
	}
	switch raw.Request.ToolName {
	case "AskUserQuestion", "ExitPlanMode":
		return false, nil
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "allow",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal full-access approval response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send full-access approval response: %w", err)
	}
	return true, nil
}

func (s *Session) handleExitPlanModeRequest(raw controlRequestEnvelope) (bool, error) {
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" || raw.Request.ToolName != "ExitPlanMode" {
		return false, nil
	}

	planMarkdown := extractExitPlanModePlan(raw.Request.Input)
	if planMarkdown != "" {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			ThreadID:  s.threadID,
			ItemID:    raw.RequestID,
			ItemType:  raw.Request.ToolName,
			Content:   planMarkdown,
			Timestamp: time.Now(),
		})
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "deny",
				"message":  "The client captured your proposed plan. Stop here and wait for the user's feedback or implementation request in a later turn.",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal exit plan mode response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send exit plan mode response: %w", err)
	}
	return true, nil
}

// handleControlCancelRequestLine processes a CLI-originated
// `control_cancel_request` envelope. The CLI emits these when an
// interrupt aborts an in-flight `can_use_tool` callback — the request
// is no longer being awaited on the CLI side, so we must clear the
// matching pending approval / user-input state without writing a
// control_response.
//
// The cancellation payload mirrors t3-code's AbortSignal handlers:
// pending approvals resolve as `decision: "cancel"` (matching
// ClaudeAdapter.ts:2764 — "User cancelled tool execution."), pending
// user-inputs resolve with empty `answers: {}` (matching
// ClaudeAdapter.ts:2612). The frontend panel listens for the matching
// EventApprovalResolved / EventUserInputResolved kind and clears.
func (s *Session) handleControlCancelRequestLine(line []byte) {
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		log.Printf("claude: malformed control_cancel_request line: %v", err)
		return
	}
	if raw.Type != "control_cancel_request" {
		// Prefix-only false positive. Cheap to verify; cheap to drop.
		return
	}
	requestID := raw.RequestID
	if requestID == "" {
		log.Printf("claude: control_cancel_request missing request_id: %s", string(line[:min(len(line), 200)]))
		return
	}
	s.cancelPendingApproval(requestID)
}

// cancelPendingApproval clears the pending approval / user-input entry
// for requestID and emits the matching resolved event so the frontend
// panel disappears. Idempotent: if the request is already resolved or
// unknown, the call is a no-op.
func (s *Session) cancelPendingApproval(requestID string) {
	s.approvalsMu.Lock()
	if s.approvalDedup.IsResolved(requestID) {
		s.approvalsMu.Unlock()
		return
	}
	pending, ok := s.pendingApprovals[requestID]
	if !ok {
		s.approvalsMu.Unlock()
		return
	}
	delete(s.pendingApprovals, requestID)
	s.approvalDedup.MarkResolved(requestID)
	resolveKind := pending.resolveKind
	s.approvalsMu.Unlock()

	metaFields := map[string]any{
		"requestId": requestID,
	}
	switch resolveKind {
	case provider.EventUserInputResolved:
		metaFields["answers"] = map[string]any{}
		metaFields["decision"] = "cancel"
	default:
		metaFields["decision"] = "cancel"
	}
	meta, _ := json.Marshal(metaFields)
	s.onEvent(provider.ProviderEvent{
		Kind:      resolveKind,
		ThreadID:  s.threadID,
		ItemID:    requestID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}
