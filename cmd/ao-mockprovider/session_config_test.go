package main

import (
	"encoding/json"
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
