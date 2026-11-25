package app

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/browser"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflowtools"
)

// TestBuiltInMCPToolSchemaBudget keeps all four built-in AO MCP servers
// safely under agy's 20,000-token customization_token_budget (estimated at
// ceil(bytes/3)) when agy loads every ACP MCP server eagerly.
func TestBuiltInMCPToolSchemaBudget(t *testing.T) {
	t.Parallel()

	threadShape := threadtools.Shape{
		Computers: []threadtools.Computer{{ID: "studio", Name: "Studio"}},
		Defaults: threadtools.SpawnDefaults{
			Provider: "claude", Model: "claude-opus-4-6", Effort: "high", Mode: "chat", RuntimeMode: "full-access",
		},
		Spawn: threadtools.SpawnShape{
			Level: 0, Limit: 2,
			Providers: []string{"claude", "codex", "agy"},
			Profiles:  []string{"Astra", "Fable", "Sol", "Opus", "Sonnet", "Terra", "Haiku", "Luna"},
		},
		MaxWaitSeconds: 150,
	}
	workflowCaller := workflowtools.Caller{MaxWaitSeconds: 150}

	type serverSpec struct {
		name         string
		tools        []map[string]any
		instructions string
	}
	servers := []serverSpec{
		{
			name:         threadMCPName,
			tools:        threadtools.New(nil).Tools(threadShape),
			instructions: threadtools.New(nil).Instructions(threadShape),
		},
		{
			name:         workflowMCPName,
			tools:        workflowtools.New(nil).Tools(workflowCaller),
			instructions: workflowtools.New(nil).Instructions(workflowCaller),
		},
		{
			name:  remoteMCPName,
			tools: remoteToolDefinitionsFor(150),
		},
		{
			name:         browser.ServerName,
			tools:        browser.ToolDefinitions(),
			instructions: browser.Instructions(),
		},
	}

	const (
		// agy's customization_token_budget is 20,000 tokens (ceil(bytes/3)).
		// Keep AO's 4 servers <= 14,000 tokens so >= 6,000 tokens remain for
		// agy's built-in tools/skills and user customizations.
		maxAgyEstimatedTokens = 14000
		maxSingleToolBytes    = 3200
		maxInstructionsBytes  = 8000
	)

	var (
		totalTools       int
		totalRawBytes    int
		totalAgyBytes    int
		totalInstrBytes  int
		largestToolName  string
		largestToolBytes int
	)

	for _, srv := range servers {
		rawBytes := 0
		agyBytes := len(fmt.Sprintf("# %s_00000000\nEager:\n", srv.name))
		for _, tool := range srv.tools {
			totalTools++
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal %s/%s: %v", srv.name, name, err)
			}
			rawBytes += len(raw)
			if len(raw) > largestToolBytes {
				largestToolBytes = len(raw)
				largestToolName = srv.name + "/" + name
			}
			if len(raw) > maxSingleToolBytes {
				t.Errorf("%s/%s schema is %d bytes, over per-tool cap %d", srv.name, name, len(raw), maxSingleToolBytes)
			}

			// Mirror agy's eager FunctionDeclaration payload (fingerprinted server
			// name prefix + description preamble + parameters schema) and <mcp_servers> line.
			agyDecl, err := json.Marshal(map[string]any{
				"name":        fmt.Sprintf("mcp_%s_00000000_%s", srv.name, name),
				"description": fmt.Sprintf("This is a tool from the %s_00000000 MCP server.\n%s", srv.name, desc),
				"parameters":  tool["inputSchema"],
			})
			if err != nil {
				t.Fatalf("marshal agy decl %s/%s: %v", srv.name, name, err)
			}
			agyBytes += len(agyDecl) + len(name) + 1
		}
		totalRawBytes += rawBytes
		totalAgyBytes += agyBytes
		totalInstrBytes += len(srv.instructions)
		t.Logf("%-18s tools=%2d raw=%6d B agy=%6d B (~%5d agy tok) instructions=%5d B",
			srv.name, len(srv.tools), rawBytes, agyBytes, (agyBytes+2)/3, len(srv.instructions))
	}

	agyTokens := (totalAgyBytes + 2) / 3
	t.Logf("TOTAL              tools=%2d raw=%6d B agy=%6d B (~%5d agy tok) instructions=%5d B (largest=%s %d B)",
		totalTools, totalRawBytes, totalAgyBytes, agyTokens, totalInstrBytes, largestToolName, largestToolBytes)

	if agyTokens > maxAgyEstimatedTokens {
		t.Errorf("built-in MCP schemas estimate %d agy tokens (%d B), over budget %d tokens", agyTokens, totalAgyBytes, maxAgyEstimatedTokens)
	}
	if totalInstrBytes > maxInstructionsBytes {
		t.Errorf("built-in MCP server instructions total %d bytes, over cap %d", totalInstrBytes, maxInstructionsBytes)
	}
}
