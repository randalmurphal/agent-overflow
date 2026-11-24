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

const maxMCPStatusOutputBytes = 2 << 20 // 2 MiB

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if rem := l.max - l.buf.Len(); rem > 0 {
		if len(p) > rem {
			_, _ = l.buf.Write(p[:rem])
		} else {
			_, _ = l.buf.Write(p)
		}
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.buf.String() }

// MCPStatusFetcher runs `agy mcp list` and projects the configured server
// statuses into AO's shared mcpstatus cache.
type MCPStatusFetcher struct {
	Binary string
	// Home decides the canonical home, whose MCP configuration Agent
	// Overflow writes. It runs inside the fetch's time limit.
	Home    HomeSource
	Env     provider.EnvInput
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

	if f.Home == nil {
		return nil, fmt.Errorf("agy mcp status: launch without a home")
	}
	home, err := f.Home(ctx)
	if err != nil {
		return nil, err
	}
	launch, err := home.Launch(f.Env)
	if err != nil {
		return nil, err
	}
	if err := launch.OnDefaultHome("mcp list"); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "mcp", "list")
	if strings.TrimSpace(f.Cwd) != "" {
		cmd.Dir = f.Cwd
	}
	cmd.Env = provider.BuildEnvironment(launch.Env)
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	stdout := &limitedBuffer{max: maxMCPStatusOutputBytes}
	stderr := &limitedBuffer{max: maxMCPStatusOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
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
