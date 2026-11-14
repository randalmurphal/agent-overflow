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
	result, err := readNativeAccount(ctx, cfg, "transfer account", false)
	if err != nil {
		return err
	}
	return checkTransferAccountResult(result)
}

// RefreshAccountToken has the Codex CLI refresh the canonical ChatGPT
// sign-in's access token (`account/read` with refreshToken), which it
// persists to `auth.json` as its own refresh does. The app-server does not
// report a failed refresh; the caller learns of one when the token is still
// refused.
func RefreshAccountToken(ctx context.Context, cfg ProbeConfig) error {
	_, err := readNativeAccount(ctx, cfg, "account token refresh", true)
	return err
}

// readNativeAccount answers `account/read` from a threadless app-server in
// the canonical home.
func readNativeAccount(ctx context.Context, cfg ProbeConfig, label string, refresh bool) (_ json.RawMessage, retErr error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "codex"
	}
	spawnCfg, cleanup, err := prepareAccountProcess(provider.SpawnConfig{
		Binary: binary, Args: codexAppServerArgs(), Dir: cfg.WorkDir,
		Env: cfg.Env, UnsetEnv: []string{"CODEX_HOME"},
	})
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	proc, err := provider.Spawn(ctx, spawnCfg)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, proc.Close()) }()
	client := &oneshotClient{proc: proc, label: label}
	if err := client.initialize(ctx, oneshotSpec{ClientName: "agent_overflow_probe"}); err != nil {
		return nil, err
	}
	return client.request(ctx, "account/read", map[string]any{"refreshToken": refresh})
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
