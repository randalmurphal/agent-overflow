package agy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
)

// DefaultMCPStatusTimeout bounds one `agy mcp list` invocation.
const DefaultMCPStatusTimeout = 15 * time.Second

// MCPStatusFetcher runs `agy mcp list` and projects the configured server
// statuses into AO's shared mcpstatus cache.
type MCPStatusFetcher struct {
	Binary  string
	Env     map[string]string
	Cwd     string
	Timeout time.Duration
}

// Fetch satisfies mcpstatus.Fetcher.
func (f *MCPStatusFetcher) Fetch(ctx context.Context, _ mcpstatus.Provider) ([]mcpstatus.ServerStatus, error) {
	bin := ResolveBinary(f.Binary)
	if strings.TrimSpace(bin) == "" {
		return nil, fmt.Errorf("agy mcp status: binary path required")
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultMCPStatusTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "mcp", "list")
	if strings.TrimSpace(f.Cwd) != "" {
		cmd.Dir = f.Cwd
	}
	launchEnv, unset := LaunchEnv(f.Env, nil)
	cmd.Env = provider.BuildEnvironment(launchEnv, unset...)
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("agy mcp list: %s", msg)
		}
		return nil, fmt.Errorf("agy mcp list: %w", err)
	}
	return ParseMCPListOutput(stdout.String(), time.Now()), nil
}

// ParseMCPListOutput parses `agy mcp list` tabular output:
//
//	NAME  TYPE  STATUS   COMMAND/URL
//	demo  http  enabled  https://example.com/mcp
//
// or "No MCP servers configured." into normalized mcpstatus entries.
func ParseMCPListOutput(raw string, now time.Time) []mcpstatus.ServerStatus {
	var out []mcpstatus.ServerStatus
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "No MCP servers") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}
		if strings.EqualFold(fields[0], "NAME") && strings.EqualFold(fields[1], "TYPE") {
			continue
		}
		name := fields[0]
		status := mcpstatus.StatusUnknown
		switch strings.ToLower(fields[2]) {
		case "enabled", "active", "ready":
			status = mcpstatus.StatusNotStarted
		case "connected":
			status = mcpstatus.StatusConnected
		case "disabled":
			status = mcpstatus.StatusDisabled
		case "failed", "error":
			status = mcpstatus.StatusFailed
		case "needs-auth", "unauthenticated":
			status = mcpstatus.StatusNeedsAuth
		}
		out = append(out, mcpstatus.ServerStatus{
			Key:       mcpstatus.Key{Provider: mcpstatus.ProviderAgy, Name: name},
			Status:    status,
			Source:    mcpstatus.SourceEphemeralFetch,
			CheckedAt: now,
		})
	}
	return out
}
