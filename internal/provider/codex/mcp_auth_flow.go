package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// MCPAuthConfig describes the threadless app-server process used only for an
// MCP OAuth round trip. No thread/start request is sent, so the flow cannot
// create provider thread state or spend model tokens.
type MCPAuthConfig struct {
	Binary         string
	WorkDir        string
	Env            map[string]string
	RequestTimeout time.Duration
}

// MCPAuthFlow owns the app-server process and its loopback OAuth listener
// until the provider sends mcpServer/oauthLogin/completed.
type MCPAuthFlow struct {
	client     *oneshotClient
	serverName string
	closeOnce  sync.Once
}

// StartMCPAuth starts a threadless app-server, asks it to begin OAuth, and
// returns the URL the caller should open. The caller must keep flow alive and
// call Wait before Close so Codex can receive the browser callback and emit
// the completion notification on this connection.
func StartMCPAuth(ctx context.Context, cfg MCPAuthConfig, serverName string) (*MCPAuthFlow, *MCPAuthResult, error) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, nil, fmt.Errorf("codex: mcpServer/oauth/login: server name required")
	}
	if err := provider.ValidateProbeWorkDir("codex", cfg.WorkDir); err != nil {
		return nil, nil, err
	}
	if cfg.RequestTimeout <= 0 {
		return nil, nil, fmt.Errorf("codex: mcp auth request timeout must be positive")
	}
	client, err := startOneshotClient(ctx, oneshotSpec{
		Binary:            cfg.Binary,
		WorkDir:           cfg.WorkDir,
		Env:               cfg.Env,
		ClientName:        "agent-overflow-mcp-auth",
		KeepNotifications: []string{"mcpServer/oauthLogin/completed"},
		Label:             "mcp auth",
	})
	if err != nil {
		return nil, nil, err
	}
	flow := &MCPAuthFlow{client: client, serverName: serverName}
	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	resp, err := client.request(requestCtx, "mcpServer/oauth/login", map[string]any{"name": serverName})
	if err != nil {
		flow.Close()
		return nil, nil, fmt.Errorf("codex: mcpServer/oauth/login %s: %w", serverName, err)
	}
	result := &MCPAuthResult{}
	if err := json.Unmarshal(resp, result); err != nil {
		flow.Close()
		return nil, nil, fmt.Errorf("codex: mcpServer/oauth/login %s: decode response: %w", serverName, err)
	}
	if strings.TrimSpace(result.AuthorizationURL) == "" {
		flow.Close()
		return nil, nil, fmt.Errorf("codex: mcpServer/oauth/login %s: empty authorizationUrl in response", serverName)
	}
	return flow, result, nil
}

// Wait blocks until this flow's completion notification arrives. A provider
// rejection is a completed flow and returns success=false with its message.
// Transport and wire failures are returned as errors.
func (f *MCPAuthFlow) Wait(ctx context.Context) (success bool, errorMessage string, err error) {
	if f == nil || f.client == nil {
		return false, "", fmt.Errorf("codex: mcp auth flow unavailable")
	}
	for {
		line, err := f.client.readLine(ctx)
		if err != nil {
			return false, "", err
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return false, "", fmt.Errorf("codex: decode mcp OAuth notification: %w", err)
		}
		if envelope.Method != "mcpServer/oauthLogin/completed" {
			continue
		}
		var completed struct {
			Name    string `json:"name"`
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(envelope.Params, &completed); err != nil {
			return false, "", fmt.Errorf("codex: decode mcp OAuth completion: %w", err)
		}
		if strings.TrimSpace(completed.Name) == "" {
			return false, "", fmt.Errorf("codex: mcp OAuth completion missing server name")
		}
		if completed.Name != f.serverName {
			continue
		}
		return completed.Success, completed.Error, nil
	}
}

// Close tears down the threadless app-server and its callback listener.
func (f *MCPAuthFlow) Close() {
	if f == nil {
		return
	}
	f.closeOnce.Do(func() {
		f.client.close()
	})
}
