package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const defaultLoginTimeout = 10 * time.Minute

type LoginConfig struct {
	Binary  string
	WorkDir string
	Env     map[string]string
	Timeout time.Duration
	OpenURL func(string) error
}

// Login starts a short-lived app-server, asks it to perform native ChatGPT
// OAuth, opens the returned authorization URL through the host browser, and
// waits for the matching completion notification. Credential storage remains
// entirely inside Codex as auth.json under the supplied CODEX_HOME.
func Login(ctx context.Context, cfg LoginConfig) error {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "codex"
	}
	if cfg.OpenURL == nil {
		return errors.New("codex: login requires a browser opener")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}
	loginCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc, err := provider.Spawn(loginCtx, provider.SpawnConfig{
		Binary:   binary,
		Args:     codexAppServerArgs(),
		Dir:      cfg.WorkDir,
		Env:      cfg.Env,
		UnsetEnv: []string{"CODEX_HOME"},
	})
	if err != nil {
		return fmt.Errorf("codex: login spawn: %w", err)
	}
	defer func() { _ = proc.Close() }()

	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		// account/login/completed is the one notification this handshake
		// waits on (waitForLoginCompletion); everything else in the
		// catalogue is noise for a login-only client.
		"params": codexInitializeParams(
			"agent_overflow_login",
			oneShotOptOutNotificationMethods("account/login/completed"),
		),
	}); err != nil {
		return fmt.Errorf("codex: login initialize: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return fmt.Errorf("codex: login initialized: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "account/login/start",
		"params": map[string]any{
			"type":                  "chatgpt",
			"codexStreamlinedLogin": false,
		},
	}); err != nil {
		return fmt.Errorf("codex: start login: %w", err)
	}

	loginID, authURL, err := waitForLoginStart(loginCtx, proc)
	if err != nil {
		return err
	}
	if err := cfg.OpenURL(authURL); err != nil {
		return fmt.Errorf("codex: open login in browser: %w", err)
	}
	return waitForLoginCompletion(loginCtx, proc, loginID)
}

func waitForLoginStart(ctx context.Context, proc *provider.Process) (string, string, error) {
	for {
		line, err := readLoginLine(ctx, proc)
		if err != nil {
			return "", "", err
		}
		var envelope struct {
			ID     *json.Number    `json:"id,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&envelope); err != nil || envelope.ID == nil {
			continue
		}
		id, err := envelope.ID.Int64()
		if err != nil || id != 2 {
			continue
		}
		if envelope.Error != nil {
			// App-server error text is provider-controlled and may echo OAuth
			// state. The numeric code is sufficient for diagnostics.
			return "", "", fmt.Errorf("codex: login start error %d", envelope.Error.Code)
		}
		var result struct {
			Type    string `json:"type"`
			LoginID string `json:"loginId"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			return "", "", fmt.Errorf("codex: decode login start: %w", err)
		}
		if result.Type != "chatgpt" || result.LoginID == "" || result.AuthURL == "" {
			return "", "", errors.New("codex: login start returned an incomplete ChatGPT flow")
		}
		return result.LoginID, result.AuthURL, nil
	}
}

func waitForLoginCompletion(ctx context.Context, proc *provider.Process, loginID string) error {
	for {
		line, err := readLoginLine(ctx, proc)
		if err != nil {
			return err
		}
		var envelope struct {
			Method string `json:"method"`
			Params struct {
				LoginID *string `json:"loginId"`
				Success bool    `json:"success"`
				Error   *string `json:"error"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil ||
			envelope.Method != "account/login/completed" ||
			envelope.Params.LoginID == nil ||
			*envelope.Params.LoginID != loginID {
			continue
		}
		if envelope.Params.Success {
			return nil
		}
		if envelope.Params.Error != nil && strings.TrimSpace(*envelope.Params.Error) != "" {
			return errors.New("codex: login failed; retry the browser flow")
		}
		return errors.New("codex: login failed")
	}
}

func readLoginLine(ctx context.Context, proc *provider.Process) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := proc.ReadLine()
		ch <- result{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("codex: login: %w", ctx.Err())
	case result := <-ch:
		if errors.Is(result.err, io.EOF) {
			return nil, errors.New("codex: app-server exited before login completed")
		}
		if result.err != nil {
			return nil, fmt.Errorf("codex: login read: %w", result.err)
		}
		return result.line, nil
	}
}
