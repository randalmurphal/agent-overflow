package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"agent-overflow/internal/provider"
)

// ProbeIdentity is the account/read-only counterpart used for external-login
// detection. It starts a fresh app-server so auth.json is loaded from disk,
// but never calls the rate-limit service or starts a model turn.
func ProbeIdentity(ctx context.Context, cfg ProbeConfig) (provider.AccountInfo, error) {
	if err := provider.ValidateProbeWorkDir("codex", cfg.WorkDir); err != nil {
		return provider.AccountInfo{}, err
	}
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc, err := provider.Spawn(probeCtx, provider.SpawnConfig{
		Binary:   binary,
		Args:     buildProbeArgs(),
		Dir:      cfg.WorkDir,
		Env:      cfg.Env,
		UnsetEnv: []string{"CODEX_HOME"},
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: identity probe spawn: %w", err)
	}
	defer func() { _ = proc.Close() }()

	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeInitializeID,
		"method":  "initialize",
		// Response-only client: it reads account/read and never waits on
		// a notification, so the whole catalogue is noise.
		"params": codexInitializeParams("agent_overflow_probe", oneShotOptOutNotificationMethods()),
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: identity probe initialize: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: identity probe initialized: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeAccountID,
		"method":  "account/read",
		"params":  map[string]any{"refreshToken": false},
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: identity probe account/read: %w", err)
	}
	return readIdentityResponse(probeCtx, proc)
}

func readIdentityResponse(ctx context.Context, proc *provider.Process) (provider.AccountInfo, error) {
	type readResult struct {
		line []byte
		err  error
	}
	results := make(chan readResult, 1)
	go func() {
		for {
			line, err := proc.ReadLine()
			select {
			case results <- readResult{line: line, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return provider.AccountInfo{}, fmt.Errorf("codex: identity probe: %w", ctx.Err())
		case result := <-results:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					return provider.AccountInfo{}, fmt.Errorf(
						"codex: identity probe: app-server exited before account/read",
					)
				}
				return provider.AccountInfo{}, fmt.Errorf("codex: identity probe read: %w", result.err)
			}
			info, matched, err := tryParseIdentityResponse(result.line)
			if err != nil {
				return provider.AccountInfo{}, err
			}
			if matched {
				return info, nil
			}
		}
	}
}

func tryParseIdentityResponse(line []byte) (provider.AccountInfo, bool, error) {
	var envelope struct {
		ID     *json.Number    `json:"id,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil || envelope.ID == nil {
		return provider.AccountInfo{}, false, nil
	}
	id, err := envelope.ID.Int64()
	if err != nil || id != probeAccountID {
		return provider.AccountInfo{}, false, nil
	}
	if envelope.Error != nil {
		return provider.AccountInfo{}, true, fmt.Errorf(
			"codex: identity probe account/read error %d: %s",
			envelope.Error.Code,
			envelope.Error.Message,
		)
	}
	info, err := decodeAccountInfo(envelope.Result)
	if err != nil {
		return provider.AccountInfo{}, true, fmt.Errorf(
			"codex: identity probe account/read response: %w",
			err,
		)
	}
	if info.APIProvider == "" {
		info.APIProvider = "openai"
	}
	return info, true, nil
}
