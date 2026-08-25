package claude

import (
	"encoding/json"
	"os"
	"strconv"
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
			if meta["api_error_enum"] != tc.enum {
				t.Fatalf("meta.api_error_enum: got %v, want %q", meta["api_error_enum"], tc.enum)
			}
			if v, ok := meta["fatal"].(bool); !ok || !v {
				t.Fatalf("meta.fatal: got %v, want true", meta["fatal"])
			}
			if v, ok := meta["expect_turn_complete"].(bool); !ok || !v {
				t.Fatalf("meta.expect_turn_complete: got %v, want true", meta["expect_turn_complete"])
			}
			wantClass := provider.FailureFatal
			if tc.enum == "rate_limit" {
				wantClass = provider.FailureTransient
			} else if tc.enum == "server_error" {
				wantClass = provider.FailureTransientAfterRetry
			}
			if errEvent.Failure == nil || errEvent.Failure.Class != wantClass || !errEvent.Failure.WaitsForTurnComplete() {
				t.Fatalf("failure = %+v, want class %q awaiting completion", errEvent.Failure, wantClass)
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

func TestParseAssistant_TopLevelErrorUsesTextContent(t *testing.T) {
	const errorText = "API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment. If it persists, check status.claude.com."
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":` + strconv.Quote(errorText) + `}]},"error":"server_error"}`)
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
	if errEvent.Content != errorText {
		t.Fatalf("Content: got %q, want %q", errEvent.Content, errorText)
	}
	var meta map[string]any
	if err := json.Unmarshal(errEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["error"] != "server_error" {
		t.Fatalf("meta.error: got %v, want server_error", meta["error"])
	}
	if v, ok := meta["expect_turn_complete"].(bool); !ok || !v {
		t.Fatalf("meta.expect_turn_complete: got %v, want true", meta["expect_turn_complete"])
	}
}

func TestParseAssistant_ErrorTextIsBounded(t *testing.T) {
	message := strings.Repeat("x", maxJoinedErrorChars+10)
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":` + strconv.Quote(message) + `}]},"error":"server_error"}`)
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
	want := strings.Repeat("x", maxJoinedErrorChars) + "..."
	if errEvent.Content != want {
		t.Fatalf("Content length/content mismatch: got len=%d want len=%d", len(errEvent.Content), len(want))
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

func TestParseAssistant_AgentTaskToolUseMeta(t *testing.T) {
	cases := []struct {
		name                 string
		toolName             string
		input                string
		wantAssistantMessage string
		wantSubagentLaunch   bool
	}{
		{
			name:                 "foreground Agent",
			toolName:             "Agent",
			input:                `{"description":"inspect"}`,
			wantAssistantMessage: "msg-inline",
			wantSubagentLaunch:   true,
		},
		{
			name:                 "foreground Task",
			toolName:             "Task",
			input:                `{"description":"inspect"}`,
			wantAssistantMessage: "msg-inline",
			wantSubagentLaunch:   true,
		},
		{
			name:                 "background Agent",
			toolName:             "Agent",
			input:                `{"description":"inspect","run_in_background":true}`,
			wantAssistantMessage: "msg-inline",
			wantSubagentLaunch:   true,
		},
		{
			name:                 "non Agent tool",
			toolName:             "Read",
			input:                `{"file_path":"foo.ts"}`,
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
			if got, _ := meta[provider.MetaSubagentLaunchKey].(bool); got != tc.wantSubagentLaunch {
				t.Fatalf("%s: got %v, want %v", provider.MetaSubagentLaunchKey, got, tc.wantSubagentLaunch)
			}
			if _, ok := meta["is_inline_subagent"]; ok {
				t.Fatalf("is_inline_subagent should not be stamped: %v", meta)
			}
			if _, ok := meta["inline_subagent_group_id"]; ok {
				t.Fatalf("inline_subagent_group_id should not be stamped: %v", meta)
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
	if errEvent.Failure == nil || errEvent.Failure.Reason != provider.FailureReasonUsageLimit ||
		errEvent.Failure.Code != "rate_limit" ||
		errEvent.Failure.Scope != provider.FailureScopeParentTurn {
		t.Fatalf("failure = %+v, want a normalized usage limit closing the parent turn", errEvent.Failure)
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

// TestParseAssistant_AdvisorCallEmitsToolStart pins the wire shape of
// the `server_tool_use` advisor call: a single content block with
// `srvtoolu_*` id and `name:"advisor"` becomes one EventToolStart
// keyed by that id, ItemType="advisor", with `advisor_model` and
// `assistant_message_id` stamped on the meta.
func TestParseAssistant_AdvisorCallEmitsToolStart(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_abc","name":"advisor","input":{}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var startEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventToolStart {
			startEvent = e
			break
		}
	}
	if startEvent.Kind != provider.EventToolStart {
		t.Fatalf("expected EventToolStart, got %+v", events)
	}
	if startEvent.ItemID != "srvtoolu_abc" {
		t.Fatalf("ItemID: got %q, want srvtoolu_abc", startEvent.ItemID)
	}
	if startEvent.ItemType != "advisor" {
		t.Fatalf("ItemType: got %q, want advisor", startEvent.ItemType)
	}
	var meta map[string]any
	if err := json.Unmarshal(startEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["toolName"] != "advisor" {
		t.Fatalf("meta.toolName: got %v, want advisor", meta["toolName"])
	}
	if meta["advisor_model"] != "claude-opus-4-7" {
		t.Fatalf("meta.advisor_model: got %v, want claude-opus-4-7", meta["advisor_model"])
	}
	if meta["assistant_message_id"] != "msg-adv" {
		t.Fatalf("meta.assistant_message_id: got %v, want msg-adv", meta["assistant_message_id"])
	}
}

// TestParseAssistant_AdvisorResultEmitsToolComplete pins the result
// block. The block arrives on a SECOND assistant envelope (same
// message id is irrelevant for parsing; this is a separate NDJSON
// line) with role=assistant content containing
// `advisor_tool_result`. We must emit EventToolComplete keyed by
// tool_use_id with the nested `content.text` as Content.
//
// Requires the call to be marked first so the result handler will
// confirm the id; we feed the call line through the same parser.
func TestParseAssistant_AdvisorResultEmitsToolComplete(t *testing.T) {
	parser := NewParser()
	callLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_abc","name":"advisor","input":{}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, callLine); err != nil {
		t.Fatalf("parse call: %v", err)
	}

	resultLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_abc","content":{"type":"advisor_result","text":"advice body here"}}]}}`)
	events, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	var completeEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			completeEvent = e
			break
		}
	}
	if completeEvent.Kind != provider.EventToolComplete {
		t.Fatalf("expected EventToolComplete, got %+v", events)
	}
	if completeEvent.ItemID != "srvtoolu_abc" {
		t.Fatalf("ItemID: got %q, want srvtoolu_abc", completeEvent.ItemID)
	}
	if completeEvent.Content != "advice body here" {
		t.Fatalf("Content: got %q, want %q", completeEvent.Content, "advice body here")
	}
	var meta map[string]any
	if err := json.Unmarshal(completeEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if v, ok := meta["is_error"].(bool); !ok || v {
		t.Fatalf("meta.is_error: got %v, want false", meta["is_error"])
	}
}

// TestParseAssistant_AdvisorResultDropsOrphan covers the defensive
// branch: an advisor_tool_result whose tool_use_id was never
// observed (parser restart mid-turn, wire drift) must be dropped
// rather than synthesise a completion against a non-existent
// launch row.
func TestParseAssistant_AdvisorResultDropsOrphan(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_unknown","content":{"type":"advisor_result","text":"x"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			t.Fatalf("orphan advisor_tool_result must not emit EventToolComplete: %+v", e)
		}
	}
}

// TestParseAssistant_ServerToolUseDropsUnknownName covers the
// forward-compat gate at the top of appendServerToolUseEvent: a
// hypothetical `web_search` or `web_fetch` server tool arriving
// under the same envelope shape must NOT be classified as advisor
// (which would stamp advisor_model on its meta and route it to
// AdvisorRow). The parser refresh that adds the new tool is the
// right place to recognise it.
func TestParseAssistant_ServerToolUseDropsUnknownName(t *testing.T) {
	cases := []struct {
		name       string
		toolName   string
		shouldEmit bool
	}{
		{"advisor", "advisor", true},
		{"web_search dropped", "web_search", false},
		{"web_fetch dropped", "web_fetch", false},
		{"empty name dropped", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := []byte(`{"type":"assistant","message":{"id":"msg-x","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_x","name":"` + tc.toolName + `","input":{}}]}}`)
			events, err := ParseLine(testThreadProto, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var hasStart bool
			for _, e := range events {
				if e.Kind == provider.EventToolStart {
					hasStart = true
				}
			}
			if hasStart != tc.shouldEmit {
				t.Fatalf("EventToolStart emitted=%v, want=%v (events=%+v)", hasStart, tc.shouldEmit, events)
			}
		})
	}
}

// TestParseAssistant_ServerToolUseDropsEmptyID covers the other
// half of the early-return guard on appendServerToolUseEvent.
func TestParseAssistant_ServerToolUseDropsEmptyID(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-x","role":"assistant","content":[{"type":"server_tool_use","id":"","name":"advisor","input":{}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolStart {
			t.Fatalf("EventToolStart on empty-id server_tool_use: %+v", e)
		}
	}
}

// TestParseAssistant_AdvisorResultDropsEmptyToolUseID covers the
// orphan-result guard distinct from the unknown-id case: a result
// with an empty tool_use_id has no row to settle.
func TestParseAssistant_AdvisorResultDropsEmptyToolUseID(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-x","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"","content":{"type":"advisor_result","text":"x"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			t.Fatalf("EventToolComplete on empty-tool_use_id result: %+v", e)
		}
	}
}

// TestParseAssistant_AdvisorResultDropsBadInnerType covers
// extractAdvisorResultText's discriminator check. A wire-drift
// inner shape (e.g. a future `advisor_error` envelope) must NOT
// surface its text as if it were a normal advisor response — the
// completion still fires so the running row settles, but with
// empty Content.
func TestParseAssistant_AdvisorResultDropsBadInnerType(t *testing.T) {
	parser := NewParser()
	callLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_xyz","name":"advisor","input":{}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, callLine); err != nil {
		t.Fatalf("parse call: %v", err)
	}
	resultLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_xyz","content":{"type":"advisor_error","text":"do not surface"}}]}}`)
	events, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	var completeEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			completeEvent = e
		}
	}
	if completeEvent.Kind != provider.EventToolComplete {
		t.Fatalf("expected EventToolComplete to fire even on bad inner type, got %+v", events)
	}
	if completeEvent.Content != "" {
		t.Fatalf("Content for unknown inner type: got %q, want empty", completeEvent.Content)
	}
}

// TestParseAssistant_AdvisorResultDropsMalformedContent covers the
// json.Unmarshal failure branch in extractAdvisorResultText. The
// completion still fires (so the running row settles) with empty
// Content.
func TestParseAssistant_AdvisorResultDropsMalformedContent(t *testing.T) {
	parser := NewParser()
	callLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_mf","name":"advisor","input":{}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, callLine); err != nil {
		t.Fatalf("parse call: %v", err)
	}
	// content as a bare string instead of the expected object shape.
	resultLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_mf","content":"raw string"}]}}`)
	events, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	var completeEvent provider.ProviderEvent
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			completeEvent = e
		}
	}
	if completeEvent.Kind != provider.EventToolComplete {
		t.Fatalf("expected EventToolComplete even on malformed content, got %+v", events)
	}
	if completeEvent.Content != "" {
		t.Fatalf("Content for malformed content: got %q, want empty", completeEvent.Content)
	}
}

// TestParseAssistant_AdvisorResultDuplicateDropsSecond pins the
// idempotency contract: a second advisor_tool_result for the same
// tool_use_id (re-delivery after reconnect, parser-state drift)
// must not emit a second EventToolComplete. The first result clears
// the correlation mark, the second falls through the orphan guard.
func TestParseAssistant_AdvisorResultDuplicateDropsSecond(t *testing.T) {
	parser := NewParser()
	callLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_dup","name":"advisor","input":{}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, callLine); err != nil {
		t.Fatalf("parse call: %v", err)
	}
	resultLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_dup","content":{"type":"advisor_result","text":"first"}}]}}`)
	first, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse first result: %v", err)
	}
	var firstCount int
	for _, e := range first {
		if e.Kind == provider.EventToolComplete {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("first result: got %d EventToolComplete events, want 1", firstCount)
	}
	second, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse second result: %v", err)
	}
	for _, e := range second {
		if e.Kind == provider.EventToolComplete {
			t.Fatalf("duplicate result emitted EventToolComplete: %+v", e)
		}
	}
}

// TestParseAssistant_AdvisorOnlyEnvelopeSuppressesUsage pins the
// load-bearing claim that the advisor's own context window does
// not flow onto the parent's context meter. An assistant envelope
// whose content is exclusively advisor blocks carries `usage` for
// the ADVISOR's call, not the parent's accumulation; surfacing it
// would clobber the meter (the fixture-captured envelope's
// cache_read_input_tokens is ~33k, easily large enough to swap
// the displayed context-used value visibly).
func TestParseAssistant_AdvisorOnlyEnvelopeSuppressesUsage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_u","name":"advisor","input":{}}],"usage":{"input_tokens":6,"cache_creation_input_tokens":305,"cache_read_input_tokens":32999,"output_tokens":8}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTokenUsage {
			t.Fatalf("advisor-only envelope must not emit EventTokenUsage (would flicker parent's meter): %+v", e)
		}
	}
}

// TestParseAssistant_MixedEnvelopeEmitsUsage pins the negation of
// the suppression rule: an envelope mixing advisor blocks with
// regular content (e.g. a `tool_use` or a `thinking` block) keeps
// usage emission for the non-advisor portion. We don't expect the
// CLI to ship this shape today, but the gate must not eat parent
// usage just because an advisor block happens to share the
// envelope.
func TestParseAssistant_MixedEnvelopeEmitsUsage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-mix","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_mix","name":"advisor","input":{}},{"type":"tool_use","id":"toolu_mix","name":"Bash","input":{"command":"true"}}],"usage":{"input_tokens":100,"cache_creation_input_tokens":200,"cache_read_input_tokens":300,"output_tokens":50}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawUsage bool
	for _, e := range events {
		if e.Kind == provider.EventTokenUsage {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatalf("mixed envelope must still emit EventTokenUsage, got %+v", events)
	}
}

// TestParseAssistant_AdvisorFixtureRoundTrip drives the captured wire
// fixture through the parser end-to-end and asserts both the call
// and the result settle as start + complete on the same id.
func TestParseAssistant_AdvisorFixtureRoundTrip(t *testing.T) {
	data, err := os.ReadFile("testdata/advisor_call.ndjson")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parser := NewParser()
	var sawStart, sawComplete bool
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if raw == "" {
			continue
		}
		events, err := parser.ParseLine(testThreadProto, []byte(raw))
		if err != nil {
			t.Fatalf("parse line: %v", err)
		}
		for _, e := range events {
			switch e.Kind {
			case provider.EventToolStart:
				if e.ItemID == "srvtoolu_01R7eTAp4mJuZ7VLg5GAAfYU" && e.ItemType == "advisor" {
					sawStart = true
				}
			case provider.EventToolComplete:
				if e.ItemID == "srvtoolu_01R7eTAp4mJuZ7VLg5GAAfYU" && strings.Contains(e.Content, "UI/integration test call") {
					sawComplete = true
				}
			}
		}
	}
	if !sawStart {
		t.Fatalf("fixture replay did not produce EventToolStart for advisor call")
	}
	if !sawComplete {
		t.Fatalf("fixture replay did not produce EventToolComplete with advisor body")
	}
}

func TestParseAssistantTextBlockIsSkipped(t *testing.T) {
	// Text blocks on the coalesced `assistant` envelope are skipped —
	// the same content streams through stream_event content_block_delta
	// (covered by TestParseStreamEventTextDelta). Emitting from both
	// paths would double the cumulative summary in triage.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTextDelta {
			t.Fatalf("assistant envelope emitted EventTextDelta for a text block: %+v", e)
		}
	}
}

func TestParseAssistantToolUseBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ItemID != "tool-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "tool-1")
	}
	if evt.ItemType != "Bash" {
		t.Errorf("itemType: got %q, want %q", evt.ItemType, "Bash")
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "Bash" {
		t.Errorf("meta toolName: got %v, want %q", meta["toolName"], "Bash")
	}
}

func TestParseAssistantExitPlanModeBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-plan-1","name":"ExitPlanMode","input":{"plan":"# Final plan\n\n- ship it"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventProposedPlan)
	}
	if evt.Content != "# Final plan\n\n- ship it" {
		t.Fatalf("content: got %q, want %q", evt.Content, "# Final plan\n\n- ship it")
	}
}

func TestParseAssistantThinkingBlockIsSkipped(t *testing.T) {
	// A thinking block on the coalesced `assistant` envelope is dropped
	// when it already streamed (stream_event thinking_delta is the sole
	// source of thinking content). The snapshot must not re-emit it on any
	// channel — neither a streaming EventThinking nor a completed block.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventThinking {
			t.Fatalf("assistant envelope emitted EventThinking for a thinking block: %+v", e)
		}
		if e.Kind == provider.EventContentBlockStop && e.ContentPresent {
			t.Fatalf("already-streamed thinking re-emitted as a completed block: %+v", e)
		}
	}
}

func TestParseAssistantWithUsage(t *testing.T) {
	// Text is dropped (already streamed — message_start marks it); usage is
	// still emitted from the assistant envelope.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (usage only), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Errorf("kind: got %q, want token_usage", events[0].Kind)
	}
	var usage provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if usage.UsedTokens != 200 {
		t.Errorf("used tokens: got %d, want 200", usage.UsedTokens)
	}
}

func TestParseAssistantMultipleBlocks(t *testing.T) {
	// Thinking and text are dropped on the assistant envelope (already
	// streamed — message_start marks them). Only the tool_use fires.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file":"x"}}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (tool_use only), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want tool_start", events[0].Kind)
	}
}
