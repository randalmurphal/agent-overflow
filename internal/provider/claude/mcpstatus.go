package claude

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
)

// MCPStatusFetcher runs `claude mcp list` and parses the human-readable
// output. We use the CLI's own subcommand rather than spawning a full
// stream-json session because the latter requires sending a user
// message (Claude only emits `system/init` per-turn) — that's a billed
// Anthropic API call. `mcp list` is local-only: it enumerates the
// configured servers and runs the same health-check connections, then
// prints results. Same work, no token cost.
//
// The Binary / Env / Cwd fields are constructor-injected so tests can
// point at a mock script under t.TempDir() without env-var trickery.
type MCPStatusFetcher struct {
	Binary  string
	Env     []string
	Cwd     string
	Timeout time.Duration // 0 → DefaultMCPStatusTimeout
}

// DefaultMCPStatusTimeout is the wall-clock ceiling for one
// `claude mcp list` invocation. The CLI fans out health checks
// concurrently; real-world runs land in 0.5–1.5s with several stdio
// servers. 15s leaves headroom for a cold-spawn npx case without
// dragging the UI for long if something hangs.
const DefaultMCPStatusTimeout = 15 * time.Second

// Fetch satisfies mcpstatus.Fetcher. The provider arg is ignored
// (always ProviderClaude); it's present so the Cache.Fetcher contract
// stays uniform across both providers.
func (f *MCPStatusFetcher) Fetch(ctx context.Context, _ mcpstatus.Provider) ([]mcpstatus.ServerStatus, error) {
	if strings.TrimSpace(f.Binary) == "" {
		return nil, fmt.Errorf("claude mcp status: binary path required")
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultMCPStatusTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, f.Binary, "mcp", "list")
	if f.Cwd != "" {
		cmd.Dir = f.Cwd
	}
	cmd.Env = provider.FilterEnvironment(f.Env, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	// TERM at the deadline, KILL only after WaitDelay — never exec's default
	// instant SIGKILL. This runs in the canonical home under a tight timeout,
	// and the CLI may be mid OAuth refresh when the deadline lands; Anthropic
	// retires the previous refresh token the moment the token endpoint
	// answers, so a SIGKILL between that answer and the CLI's credential
	// write destroys the login with no copy anywhere.
	cmd.Cancel = func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return nil
	}
	// WaitDelay also bounds the time `cmd.Wait` will spend after ctx expires
	// waiting for grandchild processes that inherit our stdout pipe
	// (e.g., a `sleep` inside a bash script) to close their fds. Without
	// it, a hung mcp-list invocation can keep Run() blocked long past
	// the context deadline because Wait blocks on the I/O-copy
	// goroutines, which block on the pipe staying open. Two seconds matches
	// the probes' kill grace: long enough for a credential write, short
	// enough that a hung fetch still returns promptly.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Even on non-zero exit, stdout might still carry parseable
		// lines — `mcp list` returns 0 in practice, but treat any
		// captured output as authoritative if present.
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("claude mcp list: %w (stderr: %s)", err, provider.SanitizeChildStderr(stderr.String()))
		}
	}
	return parseMCPList(stdout.String(), time.Now()), nil
}

// mcpListLine matches the three formats `claude mcp list` emits in
// its handler (src/cli/handlers/mcp.tsx):
//
//	${name}: ${url} (SSE) - ${status}
//	${name}: ${url} (HTTP) - ${status}
//	${name}: ${command} ${args...} - ${status}
//
// Server names can contain colons (e.g. `plugin:playwright:playwright`),
// commands can contain spaces, and status strings can contain emoji
// and spaces. The pattern anchors the suffix ` - ${status}` at the end
// of the line and treats everything before the rightmost ` - ` as the
// name+config blob. The split on the FIRST `: ` separates name from
// config — and name in practice does not contain `: ` (colon+space),
// so we're safe.
var mcpListLine = regexp.MustCompile(`^(?P<name>[^\s][^:]*(?::[^\s][^:]*)*): (?P<config>.*?) - (?P<status>.+)$`)

// parseMCPList walks the CLI output and returns one ServerStatus per
// recognised line. The header ("Checking MCP server health...") and
// the empty/footer lines are skipped. Unrecognised status strings map
// to StatusUnknown with the raw string preserved.
func parseMCPList(out string, now time.Time) []mcpstatus.ServerStatus {
	results := []mcpstatus.ServerStatus{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, " - ") {
			continue
		}
		// Anchor on the rightmost " - " so server commands containing
		// " - " in their arg lists don't confuse the split. The regex
		// would over-match name greedily otherwise.
		idx := strings.LastIndex(line, " - ")
		if idx < 0 {
			continue
		}
		left := line[:idx]
		statusRaw := strings.TrimSpace(line[idx+len(" - "):])

		// left has the form "${name}: ${config-blob}". Split on the
		// first ": " — server names can include ":" but not ": ".
		colonIdx := strings.Index(left, ": ")
		if colonIdx < 0 {
			continue
		}
		name := strings.TrimSpace(left[:colonIdx])
		if name == "" {
			continue
		}

		results = append(results, mcpstatus.ServerStatus{
			Key:       mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name},
			Status:    MCPStatusFromListLine(statusRaw),
			Raw:       statusRaw,
			Source:    mcpstatus.SourceEphemeralFetch,
			CheckedAt: now,
		})
	}
	return results
}

// MCPStatusFromRaw projects Claude's wire-level `client.type` enum onto
// the unified mcpstatus.Status. The five values seen in
// `system/init.mcp_servers[].status` and the `mcp_status`
// control_response are:
//   - "connected"    → StatusConnected
//   - "needs-auth"   → StatusNeedsAuth
//   - "failed"       → StatusFailed
//   - "pending"      → StatusStarting
//   - "disabled"     → StatusDisabled
//
// Any other value → StatusUnknown. The caller should preserve the
// original string in ServerStatus.Raw so forensics survive.
func MCPStatusFromRaw(raw string) mcpstatus.Status {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "connected":
		return mcpstatus.StatusConnected
	case "needs-auth", "needsauth":
		return mcpstatus.StatusNeedsAuth
	case "failed":
		return mcpstatus.StatusFailed
	case "pending", "starting":
		return mcpstatus.StatusStarting
	case "disabled":
		return mcpstatus.StatusDisabled
	default:
		return mcpstatus.StatusUnknown
	}
}

// MCPStatusFromListLine projects the human-readable strings emitted
// by `claude mcp list`:
//
//	"✓ Connected"            → StatusConnected
//	"! Needs authentication" → StatusNeedsAuth
//	"✗ Failed to connect"    → StatusFailed
//	"✗ Connection error"     → StatusFailed
//
// Anything else → StatusUnknown. The full string is preserved by
// the caller into ServerStatus.Raw so unfamiliar variants are still
// debuggable.
func MCPStatusFromListLine(raw string) mcpstatus.Status {
	s := strings.TrimSpace(raw)
	switch {
	case s == "✓ Connected":
		return mcpstatus.StatusConnected
	case s == "! Needs authentication":
		return mcpstatus.StatusNeedsAuth
	case s == "✗ Failed to connect", s == "✗ Connection error":
		return mcpstatus.StatusFailed
	default:
		// Tolerate emoji-free fallbacks if the CLI ever switches to
		// monochrome output without changing the status verbs.
		lowered := strings.ToLower(s)
		switch {
		case strings.Contains(lowered, "connected"):
			return mcpstatus.StatusConnected
		case strings.Contains(lowered, "needs auth"), strings.Contains(lowered, "authentication"):
			return mcpstatus.StatusNeedsAuth
		case strings.Contains(lowered, "fail"), strings.Contains(lowered, "error"):
			return mcpstatus.StatusFailed
		default:
			return mcpstatus.StatusUnknown
		}
	}
}
