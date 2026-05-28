package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/mcpstatus"
)

func TestSanitizeChildStderr_BoundsAndFlattens(t *testing.T) {
	short := sanitizeChildStderr("  ENOENT: no such file\n  ")
	if short != "ENOENT: no such file" {
		t.Fatalf("short trim got %q", short)
	}
	multiline := sanitizeChildStderr("line one\nline two\nline three")
	if strings.Contains(multiline, "\n") {
		t.Fatalf("newlines not collapsed: %q", multiline)
	}
	long := sanitizeChildStderr(strings.Repeat("A", 1024))
	if !strings.HasSuffix(long, "…(truncated)") {
		t.Fatalf("expected truncation marker, got %q (len=%d)", long, len(long))
	}
	// Cap is 256B + the truncation marker.
	if len(long) > 256+len("…(truncated)") {
		t.Fatalf("oversized output: len=%d", len(long))
	}
}

func TestParseClaudeMCPList_RealOutput(t *testing.T) {
	// Captured verbatim from `claude mcp list` on a live machine.
	// Locks the parser against the actual emitter without depending
	// on the user's local configuration.
	output := `Checking MCP server health…

claude.ai Gmail: https://gmailmcp.googleapis.com/mcp/v1 - ! Needs authentication
claude.ai Google Calendar: https://calendarmcp.googleapis.com/mcp/v1 - ! Needs authentication
claude.ai Google Drive: https://drivemcp.googleapis.com/mcp/v1 - ! Needs authentication
plugin:playwright:playwright: npx @playwright/mcp@latest - ✓ Connected
plugin:context7:context7: npx -y @upstash/context7-mcp - ✓ Connected
code-index: /home/rmurphy/go/bin/code-index-mcp serve - ✓ Connected
dispatch-atlassian: podman run --rm -i -v /home/rmurphy/local_secrets.json:/home/rimm/local_secrets.json:ro,z registry.gitlab.com/fortressinfosec/ai-devtools/dispatch:latest atlassian - ✓ Connected
dispatch-gitlab: podman run --rm -i -v /home/rmurphy/local_secrets.json:/home/rimm/local_secrets.json:ro,z -e GITLAB_TOKEN=xxxxx registry.gitlab.com/fortressinfosec/ai-devtools/dispatch:latest gitlab - ✓ Connected
`

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	results := parseMCPList(output, now)

	want := map[string]mcpstatus.Status{
		"claude.ai Gmail":              mcpstatus.StatusNeedsAuth,
		"claude.ai Google Calendar":    mcpstatus.StatusNeedsAuth,
		"claude.ai Google Drive":       mcpstatus.StatusNeedsAuth,
		"plugin:playwright:playwright": mcpstatus.StatusConnected,
		"plugin:context7:context7":     mcpstatus.StatusConnected,
		"code-index":                   mcpstatus.StatusConnected,
		"dispatch-atlassian":           mcpstatus.StatusConnected,
		"dispatch-gitlab":              mcpstatus.StatusConnected,
	}

	if len(results) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(results))
	}
	for _, got := range results {
		expected, ok := want[got.Name]
		if !ok {
			t.Errorf("unexpected server in results: %q", got.Name)
			continue
		}
		if got.Status != expected {
			t.Errorf("server %q: status = %q, want %q", got.Name, got.Status, expected)
		}
		if got.Key.Provider != mcpstatus.ProviderClaude {
			t.Errorf("server %q: provider = %q, want %q", got.Name, got.Key.Provider, mcpstatus.ProviderClaude)
		}
		if got.Source != mcpstatus.SourceEphemeralFetch {
			t.Errorf("server %q: source = %q, want %q", got.Name, got.Source, mcpstatus.SourceEphemeralFetch)
		}
		if !got.CheckedAt.Equal(now) {
			t.Errorf("server %q: CheckedAt = %v, want %v", got.Name, got.CheckedAt, now)
		}
	}
}

func TestParseClaudeMCPList_FailedAndErrorStatuses(t *testing.T) {
	output := `some-failing-server: /usr/bin/missing - ✗ Failed to connect
explody-server: ./broken.sh - ✗ Connection error
`
	results := parseMCPList(output, time.Now())
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != mcpstatus.StatusFailed {
			t.Errorf("%q: status = %q, want %q", r.Name, r.Status, mcpstatus.StatusFailed)
		}
	}
}

func TestParseClaudeMCPList_EmptyAndHeaderOnly(t *testing.T) {
	cases := []string{
		"",
		"\n\n",
		"Checking MCP server health...\n",
		"No MCP servers configured. Use `claude mcp add` to add a server.\n",
	}
	for _, c := range cases {
		if results := parseMCPList(c, time.Now()); len(results) != 0 {
			t.Errorf("input %q produced entries: %+v", c, results)
		}
	}
}

func TestParseClaudeMCPList_UnknownStatusStringPreservesRaw(t *testing.T) {
	output := `experimental: ./bin - 🌟 Quantum entangled
`
	results := parseMCPList(output, time.Now())
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Status != mcpstatus.StatusUnknown {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusUnknown)
	}
	if results[0].Raw != "🌟 Quantum entangled" {
		t.Errorf("raw = %q, want preserved string", results[0].Raw)
	}
}

func TestMCPStatusFetcher_Fetch_UsesMockBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "claude")
	script := `#!/usr/bin/env bash
cat <<EOF
Checking MCP server health…

github: /usr/local/bin/github-mcp - ✓ Connected
linear: https://linear.example/mcp - ! Needs authentication
broken-stdio: ./missing.sh - ✗ Failed to connect
EOF
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}

	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	results, err := f.Fetch(context.Background(), mcpstatus.ProviderClaude)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 entries, got %d (%+v)", len(results), results)
	}

	got := map[string]mcpstatus.Status{}
	for _, r := range results {
		got[r.Name] = r.Status
	}
	if got["github"] != mcpstatus.StatusConnected {
		t.Errorf("github status = %q", got["github"])
	}
	if got["linear"] != mcpstatus.StatusNeedsAuth {
		t.Errorf("linear status = %q", got["linear"])
	}
	if got["broken-stdio"] != mcpstatus.StatusFailed {
		t.Errorf("broken-stdio status = %q", got["broken-stdio"])
	}
}

func TestMCPStatusFetcher_Fetch_NonZeroExitButHasOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "claude")
	script := `#!/usr/bin/env bash
echo "github: ./gh - ✓ Connected"
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	results, err := f.Fetch(context.Background(), mcpstatus.ProviderClaude)
	if err != nil {
		t.Fatalf("expected output to be parsed despite non-zero exit: %v", err)
	}
	if len(results) != 1 || results[0].Status != mcpstatus.StatusConnected {
		t.Fatalf("expected one connected entry, got %+v", results)
	}
}

func TestMCPStatusFetcher_Fetch_NonZeroExitNoOutputBubblesError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "claude")
	script := `#!/usr/bin/env bash
echo "boom" >&2
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	_, err := f.Fetch(context.Background(), mcpstatus.ProviderClaude)
	if err == nil {
		t.Fatal("expected error when binary fails with no parsable output")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected stderr text in error, got %v", err)
	}
}

func TestMCPStatusFetcher_Fetch_MissingBinary(t *testing.T) {
	f := &MCPStatusFetcher{Binary: ""}
	if _, err := f.Fetch(context.Background(), mcpstatus.ProviderClaude); err == nil {
		t.Fatal("expected error for missing binary path")
	}
}

func TestMCPStatusFetcher_Fetch_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "claude")
	script := `#!/usr/bin/env bash
sleep 5
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := f.Fetch(context.Background(), mcpstatus.ProviderClaude)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout took too long: %v (expected <2s)", elapsed)
	}
	_ = fmt.Sprintf("ok") // silence unused import warning when adjusting
}

func TestMCPStatusFromRaw(t *testing.T) {
	cases := []struct {
		raw  string
		want mcpstatus.Status
	}{
		{"connected", mcpstatus.StatusConnected},
		{"Connected", mcpstatus.StatusConnected},
		{"needs-auth", mcpstatus.StatusNeedsAuth},
		{"needsauth", mcpstatus.StatusNeedsAuth},
		{"failed", mcpstatus.StatusFailed},
		{"pending", mcpstatus.StatusStarting},
		{"starting", mcpstatus.StatusStarting},
		{"", mcpstatus.StatusUnknown},
		{"garbled-string", mcpstatus.StatusUnknown},
	}
	for _, tc := range cases {
		if got := MCPStatusFromRaw(tc.raw); got != tc.want {
			t.Errorf("MCPStatusFromRaw(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestMCPStatusFromListLine(t *testing.T) {
	cases := []struct {
		raw  string
		want mcpstatus.Status
	}{
		{"✓ Connected", mcpstatus.StatusConnected},
		{"! Needs authentication", mcpstatus.StatusNeedsAuth},
		{"✗ Failed to connect", mcpstatus.StatusFailed},
		{"✗ Connection error", mcpstatus.StatusFailed},
		{"Connected", mcpstatus.StatusConnected},
		{"Needs auth", mcpstatus.StatusNeedsAuth},
		{"Authentication required", mcpstatus.StatusNeedsAuth},
		{"Some random failure", mcpstatus.StatusFailed},
		{"", mcpstatus.StatusUnknown},
		{"future-state", mcpstatus.StatusUnknown},
	}
	for _, tc := range cases {
		if got := MCPStatusFromListLine(tc.raw); got != tc.want {
			t.Errorf("MCPStatusFromListLine(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
