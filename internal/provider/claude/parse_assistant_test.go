package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseAssistant_ErrorEnumEmitsFatalEventError pins the
// behaviour for `assistant.error` envelopes — every documented enum
// value (per the agent SDK) emits a fatal EventError tagged
// `expect_turn_complete:true`. The triage router uses the
// expect_turn_complete flag to wait for the wire `result{is_error:true}`
// envelope rather than synthesising a duplicate TurnComplete; without
// the flag the router would close the turn twice in the
// assistant.error → result race.
func TestParseAssistant_ErrorEnumEmitsFatalEventError(t *testing.T) {
	cases := []struct {
		enum    string
		summary string
	}{
		{"authentication_failed", "Authentication failed"},
		{"billing_error", "Billing error"},
		{"rate_limit", "Rate limit reached"},
		{"invalid_request", "Invalid request"},
		{"server_error", "Anthropic API server error"},
		{"max_output_tokens", "Reached max output tokens"},
		{"unknown", "API error"},
	}
	for _, tc := range cases {
		t.Run(tc.enum, func(t *testing.T) {
			line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[],"error":"` + tc.enum + `"}}`)
			events, err := ParseLine(testThreadProto, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var errEvent provider.ProviderEvent
			for _, e := range events {
				if e.Kind == provider.EventError {
					errEvent = e
					break
				}
			}
			if errEvent.Kind != provider.EventError {
				t.Fatalf("expected EventError, got %+v", events)
			}
			if errEvent.Content != tc.summary {
				t.Fatalf("Content: got %q, want %q", errEvent.Content, tc.summary)
			}
			var meta map[string]any
			if err := json.Unmarshal(errEvent.Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if meta["error"] != tc.enum {
				t.Fatalf("meta.error: got %v, want %q", meta["error"], tc.enum)
			}
			if v, ok := meta["fatal"].(bool); !ok || !v {
				t.Fatalf("meta.fatal: got %v, want true", meta["fatal"])
			}
			if v, ok := meta["expect_turn_complete"].(bool); !ok || !v {
				t.Fatalf("meta.expect_turn_complete: got %v, want true", meta["expect_turn_complete"])
			}
		})
	}
}

// TestParseAssistant_ErrorPreservesTextDeltas — an `assistant.error`
// envelope can also carry `content` blocks (a brief preamble before the
// API rejection). Text deltas come from the stream-event path so the
// assistant envelope itself doesn't emit them; the error event still
// fires as the closing signal. Pins that adding the error branch
// didn't drop tool_use blocks that share the envelope.
func TestParseAssistant_ErrorWithToolUseBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}],"error":"server_error"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawTool, sawError bool
	for _, e := range events {
		switch e.Kind {
		case provider.EventToolStart:
			sawTool = true
		case provider.EventError:
			sawError = true
		}
	}
	if !sawTool {
		t.Fatalf("expected EventToolStart on tool_use block alongside error, got %+v", events)
	}
	if !sawError {
		t.Fatalf("expected EventError to fire on assistant.error alongside tool_use, got %+v", events)
	}
}

// TestParseAssistant_NoErrorFieldEmitsNoEventError covers the negative
// branch: a normal assistant envelope without `error` must not emit an
// EventError. Without this pin a stray default value or zero-string
// would surface as a phantom error row.
func TestParseAssistant_NoErrorFieldEmitsNoEventError(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventError {
			t.Fatalf("non-error assistant envelope emitted EventError: %+v", e)
		}
	}
}

func TestParseAssistant_InlineSubagentToolUseMeta(t *testing.T) {
	cases := []struct {
		name                 string
		toolName             string
		input                string
		wantInline           bool
		wantInlineGroupID    string
		wantAssistantMessage string
	}{
		{
			name:                 "foreground Agent",
			toolName:             "Agent",
			input:                `{"description":"inspect"}`,
			wantInline:           true,
			wantInlineGroupID:    "msg-inline",
			wantAssistantMessage: "msg-inline",
		},
		{
			name:                 "foreground Task",
			toolName:             "Task",
			input:                `{"description":"inspect"}`,
			wantInline:           true,
			wantInlineGroupID:    "msg-inline",
			wantAssistantMessage: "msg-inline",
		},
		{
			name:                 "background Agent",
			toolName:             "Agent",
			input:                `{"description":"inspect","run_in_background":true}`,
			wantInline:           false,
			wantAssistantMessage: "msg-inline",
		},
		{
			name:                 "non Agent tool",
			toolName:             "Read",
			input:                `{"file_path":"foo.ts"}`,
			wantInline:           false,
			wantAssistantMessage: "msg-inline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := []byte(`{"type":"assistant","message":{"id":"msg-inline","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"` + tc.toolName + `","input":` + tc.input + `}]}}`)
			events, err := ParseLine(testThreadProto, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var toolEvent provider.ProviderEvent
			for _, e := range events {
				if e.Kind == provider.EventToolStart {
					toolEvent = e
					break
				}
			}
			if toolEvent.Kind != provider.EventToolStart {
				t.Fatalf("expected EventToolStart, got %+v", events)
			}
			var meta map[string]any
			if err := json.Unmarshal(toolEvent.Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if got := meta["assistant_message_id"]; got != tc.wantAssistantMessage {
				t.Fatalf("assistant_message_id: got %v, want %q", got, tc.wantAssistantMessage)
			}
			if got := meta["is_inline_subagent"] == true; got != tc.wantInline {
				t.Fatalf("is_inline_subagent: got %v, want %v (meta=%v)", got, tc.wantInline, meta)
			}
			if tc.wantInlineGroupID == "" {
				if _, ok := meta["inline_subagent_group_id"]; ok {
					t.Fatalf("inline_subagent_group_id present for non-inline tool: %v", meta)
				}
				return
			}
			if got := meta["inline_subagent_group_id"]; got != tc.wantInlineGroupID {
				t.Fatalf("inline_subagent_group_id: got %v, want %q", got, tc.wantInlineGroupID)
			}
		})
	}
}

// TestParseAssistant_SubagentErrorCarriesParentToolUseID — when an
// assistant.error fires inside a subagent envelope (parent_tool_use_id
// set), the EventError must propagate the parent_tool_use_id so the
// triage router can attribute the failure to the parent's open turn
// rather than mistakenly opening a new top-level turn for a child id.
func TestParseAssistant_SubagentErrorCarriesParentToolUseID(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"parent-1","message":{"id":"msg-2","role":"assistant","content":[],"error":"rate_limit"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var errEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventError {
			errEvent = e
			break
		}
	}
	if errEvent.Kind != provider.EventError {
		t.Fatalf("expected EventError, got %+v", events)
	}
	if errEvent.ParentToolUseID != "parent-1" {
		t.Fatalf("ParentToolUseID: got %q, want parent-1", errEvent.ParentToolUseID)
	}
}

// TestParseAssistant_MCPToolUseNormalizesName covers Claude's
// `mcp__<server>__<tool>` wire format. The redesigned tool-call UI
// composes the body as `server.tool(args)` from a single source on
// both providers; that requires the parser to normalize the raw
// name to `MCP/<tool>` (matching the Codex `mcpToolCall` shape) and
// stamp the {server, tool} pair onto `meta.mcp` so the renderer can
// reconstruct it without re-parsing the original name.
func TestParseAssistant_MCPToolUseNormalizesName(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-mcp","role":"assistant","content":[{"type":"tool_use","id":"tool-mcp","name":"mcp__playwright__browser_click","input":{"selector":"#submit"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var toolEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventToolStart {
			toolEvent = e
			break
		}
	}
	if toolEvent.Kind != provider.EventToolStart {
		t.Fatalf("expected EventToolStart, got %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(toolEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["toolName"] != "MCP/browser_click" {
		t.Fatalf("toolName: got %v, want MCP/browser_click", meta["toolName"])
	}
	mcp, ok := meta["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("meta.mcp missing or wrong type: %#v", meta["mcp"])
	}
	if mcp["server"] != "playwright" {
		t.Fatalf("meta.mcp.server = %v, want playwright", mcp["server"])
	}
	if mcp["tool"] != "browser_click" {
		t.Fatalf("meta.mcp.tool = %v, want browser_click", mcp["tool"])
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("meta.input missing or wrong type: %#v", meta["input"])
	}
	if input["selector"] != "#submit" {
		t.Fatalf("meta.input.selector = %v, want #submit", input["selector"])
	}
}

// TestParseClaudeMCPToolName covers the parser's handling of the
// `mcp__<server>__<tool>` boundary: server names can contain single
// underscores; both halves must be non-empty; non-MCP names pass
// through unchanged.
func TestParseClaudeMCPToolName(t *testing.T) {
	cases := []struct {
		name        string
		toolName    string
		wantServer  string
		wantTool    string
		wantMatched bool
	}{
		{"playwright simple", "mcp__playwright__browser_click", "playwright", "browser_click", true},
		{"server with single underscore", "mcp__plugin_context7__query-docs", "plugin_context7", "query-docs", true},
		{"tool with double underscore in name", "mcp__server__tool__with__more", "server", "tool__with__more", true},
		{"not MCP", "Bash", "", "", false},
		{"MCP prefix but no separator", "mcp__justaserver", "", "", false},
		{"MCP prefix with empty tool", "mcp__server__", "", "", false},
		{"MCP prefix with empty server", "mcp____tool", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, tool, ok := parseClaudeMCPToolName(tc.toolName)
			if ok != tc.wantMatched {
				t.Fatalf("matched: got %v, want %v", ok, tc.wantMatched)
			}
			if server != tc.wantServer {
				t.Fatalf("server: got %q, want %q", server, tc.wantServer)
			}
			if tool != tc.wantTool {
				t.Fatalf("tool: got %q, want %q", tool, tc.wantTool)
			}
		})
	}
}

// TestErrorEnumToHumanCopy_UnknownEnumFallsBack ensures a future SDK
// enum we don't recognise still yields a non-empty human-readable
// summary so the timeline row never goes blank.
func TestErrorEnumToHumanCopy_UnknownEnumFallsBack(t *testing.T) {
	got := errorEnumToHumanCopy("future_enum_value")
	if got == "" {
		t.Fatalf("unknown enum produced empty summary")
	}
	if !strings.Contains(got, "future_enum_value") {
		t.Fatalf("fallback summary should mention the raw enum, got %q", got)
	}
}
