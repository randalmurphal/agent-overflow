package codex

import (
	"context"
	"fmt"
	"strconv"

	"agent-overflow/internal/provider"
)

type codexUserInputAnswer struct {
	Answers []string `json:"answers"`
}

func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	rpcID, result, err := buildApprovalResponseResult(resp)
	if err != nil {
		return err
	}
	return s.writeResponse(rpcID, result)
}

func buildApprovalResponseResult(resp provider.ApprovalResponse) (int64, any, error) {
	rpcID, err := parseApprovalRequestID(resp.RequestID)
	if err != nil {
		return 0, nil, err
	}

	if len(resp.Answers) > 0 {
		return rpcID, map[string]any{
			"answers": buildCodexUserInputAnswers(resp.Answers),
		}, nil
	}

	if resp.Scope != "" || resp.Permissions != nil {
		return rpcID, map[string]any{
			"scope":       resp.Scope,
			"permissions": resp.Permissions,
		}, nil
	}

	return rpcID, mapDecisionResult(resp.Decision), nil
}

func parseApprovalRequestID(requestID string) (int64, error) {
	rpcID, err := strconv.ParseInt(requestID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("codex: invalid approval request ID %q: %w", requestID, err)
	}
	return rpcID, nil
}

func buildCodexUserInputAnswers(answers map[string]provider.UserInputAnswer) map[string]codexUserInputAnswer {
	result := make(map[string]codexUserInputAnswer, len(answers))
	for questionID, answer := range answers {
		result[questionID] = codexUserInputAnswer{Answers: []string(answer)}
	}
	return result
}

func mapDecisionResult(decision string) map[string]any {
	switch decision {
	case "allow":
		return map[string]any{"decision": "accept"}
	case "allow_session":
		return map[string]any{"decision": "acceptForSession"}
	default:
		return map[string]any{"decision": "decline"}
	}
}
