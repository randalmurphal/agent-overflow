package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// AccountInfo reads the account cached by this running app-server. The request
// is local JSON-RPC and deliberately sets refreshToken=false: callers need the
// identity actually serving this process, not a network token refresh.
func (s *Session) AccountInfo(ctx context.Context) (provider.AccountInfo, error) {
	result, err := s.sendRequest(ctx, "account/read", map[string]any{
		"refreshToken": false,
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: account/read: %w", err)
	}
	return decodeAccountInfo(result)
}

func decodeAccountInfo(result json.RawMessage) (provider.AccountInfo, error) {
	var response struct {
		Account *struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			PlanType string `json:"planType"`
			// WorkspaceName (codex 0.166+, chatgpt accounts) is the selected
			// ChatGPT workspace's display name; absent or null when unknown.
			WorkspaceName string `json:"workspaceName"`
		} `json:"account"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: decode account/read: %w", err)
	}
	if response.Account == nil {
		return provider.AccountInfo{}, nil
	}
	return provider.AccountInfo{
		Email:            response.Account.Email,
		SubscriptionType: response.Account.PlanType,
		APIProvider:      "openai",
		OrgName:          strings.TrimSpace(response.Account.WorkspaceName),
	}, nil
}
