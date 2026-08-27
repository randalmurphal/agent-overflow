package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/provider"
)

type codexUserInputAnswer struct {
	Answers []string `json:"answers"`
}

func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	rpcID, result, decision, err := buildApprovalResponseResultAndDecision(resp)
	if err != nil {
		return err
	}
	if err := s.approvals.ClaimApproval(resp.RequestID, decision); err != nil {
		if errors.Is(err, provider.ErrApprovalDecisionUnavailable) {
			return err
		}
		return ErrApprovalAlreadyResolved
	}
	return s.writeResponse(rpcID, result)
}

func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	rpcID, result, err := buildUserInputResponseResult(resp)
	if err != nil {
		return err
	}
	if !s.approvals.Claim(resp.RequestID, provider.EventUserInputResolved) {
		return ErrApprovalAlreadyResolved
	}
	return s.writeResponse(rpcID, result)
}

func buildApprovalResponseResult(resp provider.ApprovalResponse) (int64, any, error) {
	rpcID, result, _, err := buildApprovalResponseResultAndDecision(resp)
	return rpcID, result, err
}

func buildApprovalResponseResultAndDecision(resp provider.ApprovalResponse) (int64, any, json.RawMessage, error) {
	rpcID, err := parseApprovalRequestID(resp.RequestID)
	if err != nil {
		return 0, nil, nil, err
	}
	permissionsMode := resp.Scope != "" || resp.Permissions != nil
	elicitationMode := resp.Elicitation != nil
	structuredDecisionMode := len(resp.DecisionValue) > 0
	if (permissionsMode && elicitationMode) ||
		(structuredDecisionMode && (permissionsMode || elicitationMode)) {
		return 0, nil, nil, fmt.Errorf("codex: approval response modes are mutually exclusive")
	}

	if permissionsMode {
		decision, _ := json.Marshal(resp.Decision)
		return rpcID, map[string]any{
			"scope":       resp.Scope,
			"permissions": resp.Permissions,
		}, decision, nil
	}

	// MCP elicitation responses use { action, content, _meta } format.
	if elicitationMode {
		decision, _ := json.Marshal(resp.Decision)
		return rpcID, map[string]any{
			"action":  resp.Elicitation.Action,
			"content": resp.Elicitation.Content,
			"_meta":   resp.Elicitation.Meta,
		}, decision, nil
	}
	if structuredDecisionMode {
		if resp.Decision != "" {
			return 0, nil, nil, fmt.Errorf("codex: approval response cannot contain both decision and decisionValue")
		}
		if !json.Valid(resp.DecisionValue) {
			return 0, nil, nil, fmt.Errorf("codex: invalid structured approval decision")
		}
		decision := append(json.RawMessage(nil), resp.DecisionValue...)
		return rpcID, map[string]any{"decision": decision}, decision, nil
	}

	// Translate frontend decision values to Codex-native vocabulary.
	decision := resp.Decision
	switch decision {
	case "allow":
		decision = "accept"
	case "allow_session":
		decision = "acceptForSession"
	case "deny":
		decision = "decline"
	}
	decisionJSON, _ := json.Marshal(decision)
	return rpcID, map[string]any{"decision": decision}, decisionJSON, nil
}

func buildUserInputResponseResult(resp provider.UserInputResponse) (int64, any, error) {
	rpcID, err := parseApprovalRequestID(resp.RequestID)
	if err != nil {
		return 0, nil, err
	}
	if _, err := provider.NormalizeUserInputDecision(resp.Decision); err != nil {
		return 0, nil, err
	}
	return rpcID, map[string]any{
		"answers": buildCodexUserInputAnswers(resp.Answers),
	}, nil
}

func parseApprovalRequestID(requestID string) (int64, error) {
	rpcID, err := strconv.ParseInt(requestID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("codex: invalid approval request ID %q: %w", requestID, err)
	}
	return rpcID, nil
}

// buildCodexUserInputAnswers shapes answers for Codex's requestUserInput
// response. Unlike Claude (which mandates a comma-joined string -- see
// claudeAskUserQuestionAnswers in the claude package), Codex's wire shape is
// {answers: []string} per question, so multi-select selections stay a
// structured slice with no join and no fidelity loss.
func buildCodexUserInputAnswers(answers map[string]provider.UserInputAnswer) map[string]codexUserInputAnswer {
	result := make(map[string]codexUserInputAnswer, len(answers))
	for questionID, answer := range answers {
		result[questionID] = codexUserInputAnswer{Answers: []string(answer)}
	}
	return result
}
