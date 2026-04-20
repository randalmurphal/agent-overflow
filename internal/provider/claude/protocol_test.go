package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const testThreadProto = "thread-proto"

// -- ParseLine tests --

func TestParseLine_SystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-6","cwd":"/tmp"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventInit)
	}
}

// TestParseLine_SystemInitSlashCommands exercises the slash_commands array on
// system.init. The Claude CLI surfaces user-configurable slash commands (from
// .claude/commands/ and built-ins) here; we round-trip them through
// SessionInfo so the frontend composer can render an autocomplete popover.
func TestParseLine_SystemInitSlashCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","slash_commands":["init","review","deploy-staging"]}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	want := []string{"init", "review", "deploy-staging"}
	if len(info.SlashCommands) != len(want) {
		t.Fatalf("SlashCommands len = %d, want %d (%v)", len(info.SlashCommands), len(want), info.SlashCommands)
	}
	for i, v := range want {
		if info.SlashCommands[i] != v {
			t.Errorf("SlashCommands[%d] = %q, want %q", i, info.SlashCommands[i], v)
		}
	}
}

// TestParseLine_SystemInitWithoutSlashCommands guards against a CLI payload
// that omits slash_commands entirely — the field must remain nil/empty and
// parsing must still succeed.
func TestParseLine_SystemInitWithoutSlashCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-6"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if len(info.SlashCommands) != 0 {
		t.Errorf("expected empty SlashCommands, got %v", info.SlashCommands)
	}
}

func TestParseLine_SystemSessionStateChanged(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"session_state_changed","data":{"state":"idle"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventSessionStatus)
	}
}

func TestParseLine_SystemApiRetry(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"attempt":2}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventSessionStatus)
	}
	if events[0].Content != "retrying" {
		t.Errorf("content: got %q, want %q", events[0].Content, "retrying")
	}
}

func TestParseLine_SystemSkippedSubtypes(t *testing.T) {
	subtypes := []string{
		"hook_started", "hook_progress", "hook_response",
		"notification", "files_persisted", "tool_use_summary",
		"memory_recall", "local_command_output",
	}
	for _, sub := range subtypes {
		line := []byte(`{"type":"system","subtype":"` + sub + `"}`)
		events, err := ParseLine(testThreadProto, line)
		if err != nil {
			t.Errorf("subtype %q: unexpected error: %v", sub, err)
		}
		if len(events) != 0 {
			t.Errorf("subtype %q: expected 0 events, got %d", sub, len(events))
		}
	}
}

func TestParseLine_SystemUnknownSubtype(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"future_feature"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown subtype, got %d", len(events))
	}
}

func TestParseLine_AssistantText(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[{"type":"text","text":"Hello"}],"role":"assistant"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTextDelta)
	}
	if events[0].Content != "Hello" {
		t.Errorf("content: got %q, want %q", events[0].Content, "Hello")
	}
}

func TestParseLine_AssistantToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}],"role":"assistant"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolStart)
	}
	if events[0].ItemType != "Bash" {
		t.Errorf("itemType: got %q, want %q", events[0].ItemType, "Bash")
	}
}

func TestParseLine_AssistantWithUsage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[],"role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":200,"cache_creation_input_tokens":30}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	hasUsage := false
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			hasUsage = true
			var usage provider.TokenUsage
			if err := json.Unmarshal(evt.Meta, &usage); err != nil {
				t.Fatalf("unmarshal usage: %v", err)
			}
			if usage.InputTokens != 100 {
				t.Errorf("InputTokens: got %d, want 100", usage.InputTokens)
			}
			if usage.CacheCreationInputTokens != 30 {
				t.Errorf("CacheCreationInputTokens: got %d, want 30", usage.CacheCreationInputTokens)
			}
		}
	}
	if !hasUsage {
		t.Error("expected a token usage event")
	}
}

// TestParseLine_AssistantUsagePricedFromInit verifies that the parser
// remembers the model reported in system/init and uses it to price
// subsequent assistant-message usage events. Before this, cost was
// computed in triage via a store round-trip — this test pins the
// provider-side annotation to the wire emission so triage can stay
// provider-agnostic.
func TestParseLine_AssistantUsagePricedFromInit(t *testing.T) {
	p := NewParser()

	initLine := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-6","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`)
	if _, err := p.ParseLine(testThreadProto, initLine); err != nil {
		t.Fatalf("parse init: %v", err)
	}

	usageLine := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[],"role":"assistant","usage":{"input_tokens":1000,"output_tokens":500}}}`)
	events, err := p.ParseLine(testThreadProto, usageLine)
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}

	var found bool
	for _, evt := range events {
		if evt.Kind != provider.EventTokenUsage {
			continue
		}
		found = true
		var usage provider.TokenUsage
		if err := json.Unmarshal(evt.Meta, &usage); err != nil {
			t.Fatalf("unmarshal usage: %v", err)
		}
		if usage.InputTokens != 1000 || usage.OutputTokens != 500 {
			t.Errorf("tokens: got input=%d output=%d", usage.InputTokens, usage.OutputTokens)
		}
		if usage.TotalCostUSD == 0 {
			t.Errorf("TotalCostUSD = 0, want priced value from CalculateCost(claude-sonnet-4-6)")
		}
	}
	if !found {
		t.Fatal("expected an EventTokenUsage emission")
	}
}

// TestParseLine_AssistantUsageNoModelNoCost guards the unpriced path:
// when the parser has never seen an init (e.g. fresh session, the
// package-level ParseLine helper) the usage event still fires but with
// TotalCostUSD == 0 rather than a bogus pricing against an empty model.
func TestParseLine_AssistantUsageNoModelNoCost(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[],"role":"assistant","usage":{"input_tokens":1000,"output_tokens":500}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind != provider.EventTokenUsage {
			continue
		}
		var usage provider.TokenUsage
		if err := json.Unmarshal(evt.Meta, &usage); err != nil {
			t.Fatalf("unmarshal usage: %v", err)
		}
		if usage.TotalCostUSD != 0 {
			t.Errorf("TotalCostUSD = %f, want 0 for unpriced (no init seen)", usage.TotalCostUSD)
		}
	}
}

// TestParseLine_UserPlainContent confirms that a `user` message whose
// `content` is a plain string (the echo of a user-typed turn input rather
// than a tool_result echo) produces no events. Only tool_result blocks in a
// list-shaped content are meaningful at this layer.
func TestParseLine_UserPlainContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"test"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for user type, got %d", len(events))
	}
}

// TestParseLine_AssistantToolUseNoBackgroundMetaByDefault verifies that a
// tool_use block without `run_in_background` does not add `is_background`
// to the emitted EventToolStart's Meta. Absence is the default.
func TestParseLine_AssistantToolUseNoBackgroundMetaByDefault(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-fg","name":"Bash","input":{"command":"ls"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["is_background"]; ok && v != false {
		t.Errorf("expected is_background to be absent or false, got %v", v)
	}
}

// TestParseLine_AssistantToolUseRunInBackground verifies that a tool_use
// with `run_in_background: true` surfaces `is_background: true` on the
// emitted EventToolStart's Meta.
func TestParseLine_AssistantToolUseRunInBackground(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"npm run dev","run_in_background":true}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolStart)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Errorf("expected is_background=true, got %v", meta["is_background"])
	}
}

// TestParseLine_UserToolResultEmitsComplete verifies that a user message
// with a `tool_result` block emits a matching EventToolComplete keyed by
// the original tool_use_id.
func TestParseLine_UserToolResultEmitsComplete(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file listing","is_error":false}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolComplete {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
	}
	if evt.ItemID != "tool-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "tool-1")
	}
	if evt.Content != "file listing" {
		t.Errorf("content: got %q, want %q", evt.Content, "file listing")
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_error"] != false {
		t.Errorf("expected is_error=false, got %v", meta["is_error"])
	}
	if _, ok := meta["is_background"]; ok {
		t.Errorf("expected is_background absent for foreground tool, got %v", meta["is_background"])
	}
}

// TestParseLine_UserToolResultMultipleBlocks verifies that a user message
// carrying multiple tool_result blocks emits one EventToolComplete for each
// block, with each ItemID mapped back to its origin tool_use_id.
func TestParseLine_UserToolResultMultipleBlocks(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tool-a","content":"first"},
		{"type":"tool_result","tool_use_id":"tool-b","content":"second"}
	]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	want := map[string]string{"tool-a": "first", "tool-b": "second"}
	for _, evt := range events {
		if evt.Kind != provider.EventToolComplete {
			t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
		}
		expected, ok := want[evt.ItemID]
		if !ok {
			t.Errorf("unexpected tool_use_id %q", evt.ItemID)
			continue
		}
		if evt.Content != expected {
			t.Errorf("content for %s: got %q, want %q", evt.ItemID, evt.Content, expected)
		}
	}
}

// TestParseLine_UserToolResultError verifies that `is_error: true` on the
// tool_result block is reflected in the emitted EventToolComplete's Meta.
func TestParseLine_UserToolResultError(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"command not found","is_error":true}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_error"] != true {
		t.Errorf("expected is_error=true, got %v", meta["is_error"])
	}
}

// TestParseLine_UserToolResultStructuredContent verifies that a tool_result
// whose content is a list of text blocks (the richer shape some tools emit)
// is flattened into a single Content string.
func TestParseLine_UserToolResultStructuredContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "hello world" {
		t.Errorf("content: got %q, want %q", events[0].Content, "hello world")
	}
}

// TestParser_BackgroundToolUseSuppressesPlaceholderToolResult verifies the
// Claude background ack path: the immediate user tool_result echo is
// informational only and must NOT terminate the tool call.
func TestParser_BackgroundToolUseSuppressesPlaceholderToolResult(t *testing.T) {
	parser := NewParser()

	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"npm run dev","run_in_background":true}}]}}`)
	startEvents, err := parser.ParseLine(testThreadProto, startLine)
	if err != nil {
		t.Fatalf("parse start: %v", err)
	}
	if len(startEvents) != 1 {
		t.Fatalf("start: expected 1 event, got %d", len(startEvents))
	}
	var startMeta map[string]any
	if err := json.Unmarshal(startEvents[0].Meta, &startMeta); err != nil {
		t.Fatalf("unmarshal start meta: %v", err)
	}
	if startMeta["is_background"] != true {
		t.Fatalf("start: expected is_background=true, got %v", startMeta["is_background"])
	}

	completeLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-bg","content":"dev server launched"}]}}`)
	completeEvents, err := parser.ParseLine(testThreadProto, completeLine)
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	if len(completeEvents) != 0 {
		t.Fatalf("complete: expected 0 events, got %d", len(completeEvents))
	}
}

// TestParser_ForegroundToolResultHasNoBackgroundFlag confirms the negative
// case: a tool_use without run_in_background followed by its tool_result
// produces a complete event whose Meta omits is_background (or has it
// false).
func TestParser_ForegroundToolResultHasNoBackgroundFlag(t *testing.T) {
	parser := NewParser()

	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-fg","name":"Bash","input":{"command":"ls"}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, startLine); err != nil {
		t.Fatalf("parse start: %v", err)
	}

	completeLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-fg","content":"file1\nfile2"}]}}`)
	completeEvents, err := parser.ParseLine(testThreadProto, completeLine)
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	if len(completeEvents) != 1 {
		t.Fatalf("complete: expected 1 event, got %d", len(completeEvents))
	}
	var meta map[string]any
	if err := json.Unmarshal(completeEvents[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["is_background"]; ok && v != false {
		t.Errorf("expected is_background absent or false for foreground tool, got %v", v)
	}
}

// TestParser_ExitCodeSurfacedFromToolUseResult verifies that when Claude
// attaches a structured `tool_use_result` sibling with an `exit_code`
// (typical for Bash), the EventToolComplete Meta exposes it so downstream
// UI can flag command failures without re-parsing the text body.
func TestParser_ExitCodeSurfacedFromToolUseResult(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"boom","is_error":true}]},"tool_use_result":{"tool_use_id":"tool-1","exit_code":127,"stdout":"","stderr":"boom"}}`)

	events, err := parser.ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["exit_code"] != float64(127) {
		t.Errorf("expected exit_code=127, got %v", meta["exit_code"])
	}
	if meta["is_error"] != true {
		t.Errorf("expected is_error=true, got %v", meta["is_error"])
	}
}

// TestParser_OutOfOrderToolResults covers a realistic mixed sequence where
// the foreground tool result arrives before a background placeholder echo.
// The foreground tool must still complete normally while the background ack
// stays suppressed.
func TestParser_OutOfOrderToolResults(t *testing.T) {
	parser := NewParser()

	// Two tool_use blocks: tool-a (background), tool-b (inline).
	startLine := []byte(`{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"tool_use","id":"tool-a","name":"Bash","input":{"command":"start daemon","run_in_background":true}},
		{"type":"tool_use","id":"tool-b","name":"Read","input":{"file_path":"/etc/hosts"}}
	]}}`)
	if _, err := parser.ParseLine(testThreadProto, startLine); err != nil {
		t.Fatalf("start parse: %v", err)
	}

	// Echo arrives with tool-b's result FIRST, then tool-a's.
	echoLine := []byte(`{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tool-b","content":"127.0.0.1"},
		{"type":"tool_result","tool_use_id":"tool-a","content":"daemon pid 1234"}
	]}}`)
	events, err := parser.ParseLine(testThreadProto, echoLine)
	if err != nil {
		t.Fatalf("echo parse: %v", err)
	}

	completes := map[string]map[string]any{}
	for _, evt := range events {
		if evt.Kind != provider.EventToolComplete {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("unmarshal meta for %s: %v", evt.ItemID, err)
		}
		completes[evt.ItemID] = meta
	}

	if len(completes) != 1 {
		t.Fatalf("expected 1 EventToolComplete, got %d", len(completes))
	}

	// tool-b was inline — no is_background flag.
	if bgB, ok := completes["tool-b"]["is_background"]; ok && bgB == true {
		t.Errorf("tool-b incorrectly marked background")
	}
	if _, ok := completes["tool-a"]; ok {
		t.Fatalf("tool-a background placeholder should stay suppressed: %+v", completes["tool-a"])
	}
}

func TestParser_TaskUpdatedTerminatesBackgroundTool(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"completed","description":"finished"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("itemID: got %q, want %q", events[0].ItemID, "tool-bg")
	}
	if events[0].Content != "finished" {
		t.Fatalf("content: got %q, want %q", events[0].Content, "finished")
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-1" {
		t.Fatalf("task_id: got %v, want task-1", meta["task_id"])
	}
	if meta["is_background"] != true {
		t.Fatalf("is_background: got %v, want true", meta["is_background"])
	}
}

func TestParser_TaskOutputEnrichesBackgroundCompletionAfterTaskUpdated(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"completed"}}`)); err != nil {
		t.Fatalf("task_updated: %v", err)
	}

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-taskoutput","name":"TaskOutput","input":{"task_id":"task-1","block":true}}]}}`)); err != nil {
		t.Fatalf("taskoutput start: %v", err)
	}

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"user","tool_use_result":{"task":{"task_id":"task-1","status":"completed","description":"Background bash is running","output_file":"/tmp/task-1.out"},"exit_code":0},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-taskoutput","content":""}]}}`))
	if err != nil {
		t.Fatalf("taskoutput result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("itemID: got %q, want %q", events[0].ItemID, "tool-bg")
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["output_file"] != "/tmp/task-1.out" {
		t.Fatalf("output_file: got %v, want /tmp/task-1.out", meta["output_file"])
	}
	if meta["exit_code"] != float64(0) {
		t.Fatalf("exit_code: got %v, want 0", meta["exit_code"])
	}
}

// TestParser_TaskNotificationEmitsCompletionWhenNoTaskUpdated covers the
// parallel-signal path: task_notification carries both task_id and
// tool_use_id inline and is treated as a completion trigger when the
// primary task_updated signal has not already fired. A later
// task_updated for the same task must NOT double-complete (see the
// dedup test below).
func TestParser_TaskNotificationEmitsCompletionWhenNoTaskUpdated(t *testing.T) {
	parser := NewParser()

	// task_started records the mapping but also emits a meta-update
	// EventToolStart so triage can persist task_id ↔ tool_use_id.
	startEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`))
	if err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if len(startEvents) != 1 || startEvents[0].Kind != provider.EventToolStart {
		t.Fatalf("expected 1 EventToolStart from task_started, got %+v", startEvents)
	}
	if startEvents[0].ItemID != "tool-bg" {
		t.Fatalf("task_started ItemID: got %q, want tool-bg", startEvents[0].ItemID)
	}

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-1","tool_use_id":"tool-bg","status":"completed","summary":"done"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventToolComplete from task_notification, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("ItemID: got %q, want tool-bg", events[0].ItemID)
	}
}

// TestParser_TaskStartedEmitsBothIDs (Bug A) verifies that when Claude
// fires `system/task_started`, the adapter emits an EventToolStart that
// carries BOTH ids — ItemID = tool_use_id so triage finds the existing
// tool_call row, and Meta.task_id so the row's items.meta captures the
// mapping. Persisting the mapping is the only way a later task_updated
// on a fresh adapter session (in-memory map lost) can correlate back.
func TestParser_TaskStartedEmitsBothIDs(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-42","tool_use_id":"tool-bg-7"}`))
	if err != nil {
		t.Fatalf("task_started parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ItemID != "tool-bg-7" {
		t.Fatalf("ItemID: got %q, want tool-bg-7", evt.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-42" {
		t.Fatalf("Meta.task_id: got %v, want task-42", meta["task_id"])
	}
}

// TestParser_TaskStartedSkipsWhenIDsAreEmpty verifies the adapter drops
// a malformed task_started (missing task_id or tool_use_id) rather than
// emitting a ghost EventToolStart. An empty ItemID would not correlate
// to any tool_call row in triage.
func TestParser_TaskStartedSkipsWhenIDsAreEmpty(t *testing.T) {
	parser := NewParser()

	cases := []string{
		`{"type":"system","subtype":"task_started","task_id":"task-1"}`,      // missing tool_use_id
		`{"type":"system","subtype":"task_started","tool_use_id":"tool-bg"}`, // missing task_id
		`{"type":"system","subtype":"task_started"}`,                         // both missing
	}
	for _, line := range cases {
		events, err := parser.ParseLine(testThreadProto, []byte(line))
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if len(events) != 0 {
			t.Errorf("expected 0 events for malformed %q, got %d", line, len(events))
		}
	}
}

// TestParser_FullTaskLifecycleEmitsSingleCompletion (Bug B happy path)
// covers the canonical sequence — task_started, task_updated(completed),
// then a delayed task_notification. The notification must dedup against
// the already-emitted completion via completedToolUseIDs.
func TestParser_FullTaskLifecycleEmitsSingleCompletion(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	updatedEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"completed","description":"ok"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(updatedEvents) != 1 || updatedEvents[0].Kind != provider.EventToolComplete {
		t.Fatalf("task_updated: expected 1 EventToolComplete, got %+v", updatedEvents)
	}

	notifyEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-1","tool_use_id":"tool-bg","status":"completed","summary":"dup"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifyEvents) != 0 {
		t.Fatalf("expected 0 events from duplicate task_notification, got %+v", notifyEvents)
	}
}

// TestParser_TaskUpdatedAloneEmitsCompletion (Bug B) covers the fresh-
// session variant: no task_started is ever observed, so the adapter's
// in-memory map is empty. task_updated carries only task_id inline —
// the adapter emits an EventToolComplete with empty ItemID and
// Meta.task_id so triage can resolve the row via items.meta.
func TestParser_TaskUpdatedAloneEmitsCompletion(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-lost","patch":{"status":"completed","description":"done"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventToolComplete, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "" {
		t.Fatalf("ItemID: got %q, want empty (triage resolves via meta.task_id)", events[0].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-lost" {
		t.Fatalf("meta.task_id: got %v, want task-lost", meta["task_id"])
	}
}

// TestParser_TaskNotificationAloneEmitsCompletion (Bug B) covers the
// other half of the fresh-session variant — task_started never fired,
// task_updated was missed (event-queue drop), and the notification
// signal arrives. Because task_notification carries tool_use_id inline
// it is self-sufficient: the adapter emits a completion keyed on that id.
func TestParser_TaskNotificationAloneEmitsCompletion(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-x","tool_use_id":"tool-bg-x","status":"completed","summary":"cleaned"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventToolComplete, got %d", len(events))
	}
	if events[0].ItemID != "tool-bg-x" {
		t.Fatalf("ItemID: got %q, want tool-bg-x", events[0].ItemID)
	}
	// A subsequent task_updated for the same task must NOT emit a
	// second completion — the notification has already deduped it.
	dupEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-x","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(dupEvents) != 0 {
		t.Fatalf("expected 0 events from duplicate task_updated after notification, got %+v", dupEvents)
	}
}

// TestParser_TaskNotificationFirstThenUpdatedIsDeduped (Bug B) reverses
// the arrival order: notification fires first (with both ids), then
// task_updated arrives. The completion must fire once total — the
// update is suppressed by the completedTasks / completedToolUseIDs sets.
func TestParser_TaskNotificationFirstThenUpdatedIsDeduped(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu1"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	notifyEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu1","status":"completed"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifyEvents) != 1 {
		t.Fatalf("expected 1 EventToolComplete from first-arriving notification, got %d", len(notifyEvents))
	}

	updatedEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(updatedEvents) != 0 {
		t.Fatalf("expected 0 events from duplicate task_updated, got %+v", updatedEvents)
	}
}

// TestParser_CloseClearsState verifies Parser.Close empties the dedup
// sets. Required so completedToolUseIDs does not leak across session
// teardown / restart within the same parser instance.
func TestParser_CloseClearsState(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-close","tool_use_id":"tool-close"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-close","patch":{"status":"completed"}}`)); err != nil {
		t.Fatalf("task_updated: %v", err)
	}

	// Before Close, a duplicate task_updated should be suppressed.
	parser.Close()

	// After Close, all dedup state is gone. A fresh task_started with
	// the same id must succeed (no lingering "already completed").
	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-close","tool_use_id":"tool-close"}`))
	if err != nil {
		t.Fatalf("task_started after close: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected fresh task_started to emit after Close, got %d events", len(events))
	}
}

func TestParseLine_ResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","is_error":false,"session_id":"s1"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
}

func TestParseLine_ResultError(t *testing.T) {
	line := []byte(`{"type":"result","is_error":true,"error":"something failed"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
	if events[0].Content != "something failed" {
		t.Errorf("content: got %q, want %q", events[0].Content, "something failed")
	}
}

func TestParseLine_StreamEventTextDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "world" {
		t.Errorf("content: got %q, want %q", events[0].Content, "world")
	}
}

func TestParseLine_StreamEventNoDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for stream event without delta text, got %d", len(events))
	}
}

func TestParseLine_ControlRequestCanUseTool(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
	if events[0].ItemID != "req-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "req-1")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(events[0].Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.ToolName != "Bash" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "Bash")
	}
}

func TestParseLine_ControlRequestNonToolSubtype(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"other_subtype"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-tool control request, got %d", len(events))
	}
}

func TestParseLine_UnknownType(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","data":{}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParseLine_InvalidJSON(t *testing.T) {
	_, err := ParseLine(testThreadProto, []byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseLine_MissingType(t *testing.T) {
	_, err := ParseLine(testThreadProto, []byte(`{"data":"something"}`))
	if err == nil {
		t.Error("expected error for missing type field")
	}
}

func TestParseLine_SystemToolProgressDropped(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress","item_id":"item-1","progress":{"percent":50}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected tool_progress to be dropped, got %d event(s)", len(events))
	}
}

func TestParseLine_SystemCompactBoundary(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","data":{"usedTokens":5000,"maxTokens":200000}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventCompactBoundary)
	}
}
