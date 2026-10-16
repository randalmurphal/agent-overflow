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

// The override and the app guide reach Claude as files; the mock reads them
// at launch the way the CLI does, so a test asserts the text the model would
// see.
func TestClaudeSessionConfigReportsThePromptFilesAndSnapshotFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "append.txt")
	if err := os.WriteFile(path, []byte("APPENDED GUIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "system.txt")
	if err := os.WriteFile(replacement, []byte("REPLACED PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := claudeSessionConfig([]string{
		"--system-prompt-file", replacement,
		"--append-system-prompt-file", path,
		"--system-prompt-snapshot", "off",
	})
	if config.SystemPrompt != "REPLACED PROMPT" {
		t.Errorf("SystemPrompt = %q, want the file's content", config.SystemPrompt)
	}
	if config.AppendSystemPrompt != "APPENDED GUIDE" {
		t.Errorf("AppendSystemPrompt = %q, want the file's content", config.AppendSystemPrompt)
	}
	if config.SystemPromptSnapshot != "off" {
		t.Errorf("SystemPromptSnapshot = %q, want off", config.SystemPromptSnapshot)
	}

	bare := claudeSessionConfig(nil)
	if bare.SystemPrompt != "" || bare.AppendSystemPrompt != "" || bare.SystemPromptSnapshot != "" {
		t.Errorf("flags omitted, yet config = %+v", bare)
	}
	missing := claudeSessionConfig([]string{"--append-system-prompt-file", filepath.Join(dir, "absent.txt")})
	if !strings.Contains(missing.AppendSystemPrompt, "read prompt file") {
		t.Errorf("an unreadable prompt file must fail an assertion loudly, got %q", missing.AppendSystemPrompt)
	}
}

// The command timeout ceiling reaches Claude through the environment, so the
// report reads it from there; absent stays "" so a spec can assert absence.
func TestClaudeSessionConfigReportsTheCommandTimeoutEnv(t *testing.T) {
	t.Setenv("BASH_MAX_TIMEOUT_MS", "18000000")
	if got := claudeSessionConfig(nil).BashMaxTimeoutMs; got != "18000000" {
		t.Fatalf("BashMaxTimeoutMs = %q, want 18000000", got)
	}
	t.Setenv("BASH_MAX_TIMEOUT_MS", "")
	if got := claudeSessionConfig(nil).BashMaxTimeoutMs; got != "" {
		t.Fatalf("BashMaxTimeoutMs = %q with the variable empty, want empty", got)
	}
}
