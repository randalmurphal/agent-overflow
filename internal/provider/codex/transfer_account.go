package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"agent-overflow/internal/provider"
)

// CheckTransferAccount reads native account availability without refreshing a
// token, fetching usage, opening a thread or running a model. A configured
// custom endpoint may explicitly need no OpenAI account.
func CheckTransferAccount(ctx context.Context, cfg ProbeConfig) error {
	if err := provider.ValidateProbeWorkDir("codex transfer account", cfg.WorkDir); err != nil {
		return err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := startOneshotClient(ctx, oneshotSpec{Binary: cfg.Binary, WorkDir: cfg.WorkDir, Env: cfg.Env, ClientName: "agent_overflow_probe", Label: "transfer account"})
	if err != nil {
		return err
	}
	defer client.close()
	result, err := client.request(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return err
	}
	return checkTransferAccountResult(result)
}

func checkTransferAccountResult(result json.RawMessage) error {
	var response struct {
		Required *bool `json:"requiresOpenaiAuth"`
		Account  *struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return err
	}
	if response.Required != nil && !*response.Required {
		return nil
	}
	if response.Account != nil && strings.TrimSpace(response.Account.Type) != "" {
		return nil
	}
	if response.Required == nil {
		return errors.New("Could not verify this computer's Codex account. Update Codex and retry.")
	}
	return errors.New("Sign in to Codex on this computer before receiving the conversation.")
}
