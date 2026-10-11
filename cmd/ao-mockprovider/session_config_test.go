package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeSessionConfigReportsSortedMCPServerNames(t *testing.T) {
	config := claudeSessionConfig([]string{
		"--mcp-config", `{"mcpServers":{"zeta":{"url":"http://127.0.0.1/secret"},"alpha":{"command":"tool"}}}`,
	})
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(config.MCPServers, want) {
		t.Fatalf("MCP servers = %v, want %v", config.MCPServers, want)
	}
}

func TestCodexSessionConfigReportsSortedMCPServerNames(t *testing.T) {
	params := json.RawMessage(`{"config":{"mcp_servers":{"zeta":{"url":"http://127.0.0.1/secret"},"alpha":{"command":"tool"}}}}`)
	if got, want := codexMCPServerNames(params), []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP servers = %v, want %v", got, want)
	}
}

func TestSessionConfigDoesNotRetainMCPDetails(t *testing.T) {
	config := claudeSessionConfig([]string{
		"--mcp-config", `{"mcpServers":{"browser":{"url":"http://127.0.0.1/token","headers":{"Authorization":"secret"}}}}`,
	})
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"127.0.0.1", "token", "Authorization", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("session config leaked %q: %s", forbidden, encoded)
		}
	}
}

// The app guide reaches Claude as a file; the mock reads it at launch the way
// the CLI does, so a test asserts the text the model would see.
func TestClaudeSessionConfigReportsTheAppendPromptAndSnapshotFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "append.txt")
	if err := os.WriteFile(path, []byte("APPENDED GUIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := claudeSessionConfig([]string{
		"--append-system-prompt-file", path,
		"--system-prompt-snapshot", "off",
	})
	if config.AppendSystemPrompt != "APPENDED GUIDE" {
		t.Errorf("AppendSystemPrompt = %q, want the file's content", config.AppendSystemPrompt)
	}
	if config.SystemPromptSnapshot != "off" {
		t.Errorf("SystemPromptSnapshot = %q, want off", config.SystemPromptSnapshot)
	}

	bare := claudeSessionConfig(nil)
	if bare.AppendSystemPrompt != "" || bare.SystemPromptSnapshot != "" {
		t.Errorf("flags omitted, yet config = %+v", bare)
	}
	missing := claudeSessionConfig([]string{"--append-system-prompt-file", filepath.Join(dir, "absent.txt")})
	if !strings.Contains(missing.AppendSystemPrompt, "read prompt file") {
		t.Errorf("an unreadable prompt file must fail an assertion loudly, got %q", missing.AppendSystemPrompt)
	}
}
