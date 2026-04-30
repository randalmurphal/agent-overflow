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
	// Text blocks on the assistant envelope are intentionally skipped —
	// stream_event content_block_delta is the sole source of text
	// deltas (see parse_assistant.go and parse_stream.go). The
	// corresponding stream-path test is TestParseStreamEventTextDelta.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[{"type":"text","text":"Hello"}],"role":"assistant"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTextDelta {
			t.Fatalf("assistant envelope emitted EventTextDelta: %+v", e)
		}
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

// TestParseLine_AssistantAskUserQuestionEmitsToolStart pins the
// behaviour that AskUserQuestion's tool_use block emits a generic
// EventToolStart timeline row. AskUserQuestion is also surfaced via
// the parallel can_use_tool control_request as EventUserInputRequest
// (driving the in-composer answer panel); the timeline row is the
// persisted historical record that flips running → completed when the
// user submits an answer.
func TestParseLine_AssistantAskUserQuestionEmitsToolStart(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-uq","name":"AskUserQuestion","input":{"questions":[{"id":"q","header":"Pick","question":"a or b?","options":[{"label":"a","description":"opt a"},{"label":"b","description":"opt b"}]}]}}]}}`)
	events, err := parser.ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var start *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventToolStart {
			start = &events[i]
			break
		}
	}
	if start == nil {
		t.Fatalf("AskUserQuestion did not emit EventToolStart: events=%+v", events)
	}
	if start.ItemType != "AskUserQuestion" {
		t.Errorf("itemType: got %q, want %q", start.ItemType, "AskUserQuestion")
	}
	if start.ItemID != "tool-uq" {
		t.Errorf("itemID: got %q, want %q", start.ItemID, "tool-uq")
	}
}

// TestParseLine_UserToolResultForAskUserQuestionEmitsCompletion pairs with
// the assistant-side emission: when the model later echoes the
// AskUserQuestion answers as a tool_result, that block emits an
// EventToolComplete that flips the timeline row to completed. This is
// what makes the asked-and-answered Q&A persist as part of the chat
// history and survive forks.
func TestParseLine_UserToolResultForAskUserQuestionEmitsCompletion(t *testing.T) {
	parser := NewParser()
	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-uq","name":"AskUserQuestion","input":{"questions":[{"id":"q","header":"Pick","question":"a or b?","options":[{"label":"a","description":"opt a"},{"label":"b","description":"opt b"}]}]}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, startLine); err != nil {
		t.Fatalf("parse start: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-uq","content":"User answered: a"}]}}`)
	events, err := parser.ParseLine(testThreadProto, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	var complete *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventToolComplete {
			complete = &events[i]
			break
		}
	}
	if complete == nil {
		t.Fatalf("AskUserQuestion tool_result did not emit EventToolComplete: events=%+v", events)
	}
	if complete.ItemID != "tool-uq" {
		t.Errorf("itemID: got %q, want %q", complete.ItemID, "tool-uq")
	}
}

// TestParseAskUserQuestions_PreservesPreview confirms the option preview
// field round-trips through parseAskUserQuestions. Claude Code's tool
// schema lets each option carry an optional `preview` for side-by-side
// mockup/code comparison; the wire path must not silently drop it.
func TestParseAskUserQuestions_PreservesPreview(t *testing.T) {
	input := []byte(`{"questions":[{"id":"q","header":"Pick","question":"layout?","options":[{"label":"a","description":"opt a","preview":"# Option A\n\nlayout 1"},{"label":"b","description":"opt b","preview":"# Option B\n\nlayout 2"}]}]}`)
	got := parseAskUserQuestions(input)
	if len(got) != 1 {
		t.Fatalf("questions: got %d, want 1", len(got))
	}
	if len(got[0].Options) != 2 {
		t.Fatalf("options: got %d, want 2", len(got[0].Options))
	}
	if got[0].Options[0].Preview != "# Option A\n\nlayout 1" {
		t.Errorf("option[0].Preview: got %q", got[0].Options[0].Preview)
	}
	if got[0].Options[1].Preview != "# Option B\n\nlayout 2" {
		t.Errorf("option[1].Preview: got %q", got[0].Options[1].Preview)
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
			var usage provider.ContextWindow
			if err := json.Unmarshal(evt.Meta, &usage); err != nil {
				t.Fatalf("unmarshal usage: %v", err)
			}
			if usage.UsedTokens != 330 {
				t.Errorf("UsedTokens: got %d, want 330", usage.UsedTokens)
			}
		}
	}
	if !hasUsage {
		t.Error("expected a token usage event")
	}
}

func TestParseLine_AssistantUsageExcludesOutputFromContextWindow(t *testing.T) {
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
		var usage provider.ContextWindow
		if err := json.Unmarshal(evt.Meta, &usage); err != nil {
			t.Fatalf("unmarshal usage: %v", err)
		}
		if usage.UsedTokens != 1000 {
			t.Errorf("UsedTokens: got %d, want 1000", usage.UsedTokens)
		}
	}
	if !found {
		t.Fatal("expected an EventTokenUsage emission")
	}
}

func TestParseLine_SubagentAssistantUsageDoesNotUpdateParentContext(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"toolu-parent","message":{"id":"msg-1","content":[],"role":"assistant","usage":{"input_tokens":1000,"output_tokens":500}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("subagent assistant usage emitted parent context update: %+v", evt)
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
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"pnpm run dev","run_in_background":true}}]}}`)
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

// TestParser_BackgroundToolUsePlaceholderCarriesIsBackgroundMeta
// verifies the universal tool-lifecycle invariant (invariant 20):
// every `tool_result` — including the backgrounded placeholder —
// emits exactly one `EventToolComplete`. The placeholder's completion
// carries `is_background: true` in meta so triage keeps the launch
// row at `status=running` per spec; the sibling `tool_completion`
// row later lands via `EventBackgroundTaskTerminal`.
func TestParser_BackgroundToolUsePlaceholderCarriesIsBackgroundMeta(t *testing.T) {
	parser := NewParser()

	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"pnpm run dev","run_in_background":true}}]}}`)
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
	if len(completeEvents) != 1 {
		t.Fatalf("complete: expected 1 event, got %d", len(completeEvents))
	}
	if completeEvents[0].Kind != provider.EventToolComplete {
		t.Fatalf("complete kind: got %q, want %q", completeEvents[0].Kind, provider.EventToolComplete)
	}
	if completeEvents[0].ItemID != "tool-bg" {
		t.Fatalf("complete ItemID: got %q, want tool-bg", completeEvents[0].ItemID)
	}
	var completeMeta map[string]any
	if err := json.Unmarshal(completeEvents[0].Meta, &completeMeta); err != nil {
		t.Fatalf("unmarshal complete meta: %v", err)
	}
	if completeMeta["is_background"] != true {
		t.Fatalf("complete: expected is_background=true, got %v", completeMeta["is_background"])
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

// TestParser_OutOfOrderToolResults covers a mixed sequence: a
// backgrounded Bash and an inline Read in the same assistant message,
// with tool_result blocks arriving for the inline tool first. Under
// the universal tool-lifecycle invariant both tool_use_ids receive an
// EventToolComplete; the backgrounded placeholder carries
// `is_background:true` in meta so triage keeps its launch row at
// status=running (per spec), while the inline tool's completion has
// no background flag.
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

	if len(completes) != 2 {
		t.Fatalf("expected 2 EventToolComplete (one per tool_use_id), got %d: %+v", len(completes), completes)
	}

	// tool-b was inline — no is_background flag.
	if bgB, ok := completes["tool-b"]["is_background"]; ok && bgB == true {
		t.Errorf("tool-b incorrectly marked background")
	}
	// tool-a is the backgrounded placeholder — is_background must be true.
	bgA, ok := completes["tool-a"]
	if !ok {
		t.Fatalf("expected a completion for backgrounded tool-a placeholder")
	}
	if bgA["is_background"] != true {
		t.Errorf("tool-a expected is_background=true, got %v", bgA["is_background"])
	}
}

// TestParser_TaskUpdatedEmitsBackgroundTaskTerminal verifies that a
// terminal `system/task_updated` emits `EventBackgroundTaskTerminal`
// (NOT `EventToolComplete` — that event belongs to the tool
// lifecycle and was already emitted by the placeholder echo). The
// terminal carries `task_id`, the resolved `tool_use_id`, and the
// normalized `status`.
func TestParser_TaskUpdatedEmitsBackgroundTaskTerminal(t *testing.T) {
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
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventBackgroundTaskTerminal)
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
	if meta["tool_use_id"] != "tool-bg" {
		t.Fatalf("tool_use_id: got %v, want tool-bg", meta["tool_use_id"])
	}
	if meta["status"] != "completed" {
		t.Fatalf("status: got %v, want completed", meta["status"])
	}
}

// TestParser_TaskOutputEmitsOwnCompleteAndBackgroundTerminal verifies
// the TaskOutput two-event shape: every `tool_result` — including
// TaskOutput's own — emits an `EventToolComplete` for its own id
// (invariant 20), AND when the block carries a terminal
// `tool_use_result.task` it additionally emits
// `EventBackgroundTaskTerminal` keyed to the backgrounded task's
// original tool_use_id with the richer enrichment (output_file,
// exit_code).
func TestParser_TaskOutputEmitsOwnCompleteAndBackgroundTerminal(t *testing.T) {
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
	if len(events) != 2 {
		t.Fatalf("expected 2 events (ToolComplete + BackgroundTaskTerminal), got %d: %+v", len(events), events)
	}
	// First: own-id EventToolComplete for TaskOutput's tool_use_id.
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("events[0].Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-taskoutput" {
		t.Fatalf("events[0].ItemID: got %q, want tool-taskoutput", events[0].ItemID)
	}
	// Second: EventBackgroundTaskTerminal carrying the backgrounded
	// task's original tool_use_id and the richer enrichment payload.
	if events[1].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("events[1].Kind: got %q, want %q", events[1].Kind, provider.EventBackgroundTaskTerminal)
	}
	if events[1].ItemID != "tool-bg" {
		t.Fatalf("events[1].ItemID: got %q, want tool-bg", events[1].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[1].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["output_file"] != "/tmp/task-1.out" {
		t.Fatalf("output_file: got %v, want /tmp/task-1.out", meta["output_file"])
	}
	if meta["exit_code"] != float64(0) {
		t.Fatalf("exit_code: got %v, want 0", meta["exit_code"])
	}
	if meta["task_id"] != "task-1" {
		t.Fatalf("task_id: got %v, want task-1", meta["task_id"])
	}
}

// TestParser_TaskNotificationEmitsNotificationOnly verifies invariant 21:
// `system/task_notification` is NOT a completion source and emits only a
// non-lifecycle notification event. The authoritative task-terminal signals are
// `system/task_updated` (basic) and TaskOutput `tool_use_result.task`
// (enriched). See docs/references/claude-wire.md §task_notification.
func TestParser_TaskNotificationEmitsNotificationOnly(t *testing.T) {
	parser := NewParser()

	startEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`))
	if err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if len(startEvents) != 1 || startEvents[0].Kind != provider.EventToolStart {
		t.Fatalf("expected 1 EventToolStart from task_started, got %+v", startEvents)
	}

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-1","tool_use_id":"tool-bg","status":"completed","summary":"done"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("expected 1 EventBackgroundTaskNotification, got %d: %+v", len(events), events)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("notification ItemID: got %q, want tool-bg", events[0].ItemID)
	}
	if events[0].Content != "done" {
		t.Fatalf("notification content: got %q, want done", events[0].Content)
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

// TestParser_FullTaskLifecycleEmitsSingleBackgroundTerminal covers
// the canonical sequence — task_started (meta-only EventToolStart),
// task_updated(completed) (one EventBackgroundTaskTerminal), then
// task_notification (notification only). Any idempotent fold of later
// terminals into the sibling row is triage's job.
func TestParser_FullTaskLifecycleEmitsSingleBackgroundTerminal(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"tool-bg"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	updatedEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"completed","description":"ok"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(updatedEvents) != 1 || updatedEvents[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("task_updated: expected 1 EventBackgroundTaskTerminal, got %+v", updatedEvents)
	}

	notifyEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-1","tool_use_id":"tool-bg","status":"completed","summary":"dup"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifyEvents) != 1 || notifyEvents[0].Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("expected 1 EventBackgroundTaskNotification from task_notification, got %+v", notifyEvents)
	}
}

// TestParser_TaskUpdatedAloneEmitsBackgroundTerminal covers the
// fresh-session variant: no task_started is ever observed, so the
// adapter's in-memory map is empty. task_updated carries only task_id
// inline — the adapter emits an EventBackgroundTaskTerminal with
// empty ItemID and Meta.task_id so triage can resolve the row via
// items.meta.task_id.
func TestParser_TaskUpdatedAloneEmitsBackgroundTerminal(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-lost","patch":{"status":"completed","description":"done"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventBackgroundTaskTerminal, got %d", len(events))
	}
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventBackgroundTaskTerminal)
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
	if _, ok := meta["tool_use_id"]; ok {
		t.Fatalf("tool_use_id should be absent when map is empty, got %v", meta["tool_use_id"])
	}
}

// TestParser_TaskNotificationOnlyFiresNoTerminal is the reinforcement
// case for invariant 21: even when task_notification is the ONLY
// terminal signal seen (e.g. task_updated was dropped), the parser
// emits only a notification. Triage's force-close safety net or the idempotent
// upsert by a later replay will handle the row eventually.
func TestParser_TaskNotificationOnlyFiresNoTerminal(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"task-x","tool_use_id":"tool-bg-x","status":"completed","summary":"cleaned"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("expected 1 EventBackgroundTaskNotification from task_notification, got %+v", events)
	}

	// A subsequent task_updated for the same task DOES emit the
	// terminal — notification did not suppress it.
	terminalEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-x","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(terminalEvents) != 1 || terminalEvents[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("expected 1 EventBackgroundTaskTerminal from task_updated, got %+v", terminalEvents)
	}
}

// TestParser_TaskNotificationFirstThenUpdatedEmitsTerminal reverses
// the arrival order: notification fires first, then
// task_updated arrives. The terminal fires on task_updated, not
// blocked by the earlier notification.
func TestParser_TaskNotificationFirstThenUpdatedEmitsTerminal(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu1"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	notifyEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu1","status":"completed"}`))
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifyEvents) != 1 || notifyEvents[0].Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("expected 1 EventBackgroundTaskNotification from task_notification, got %+v", notifyEvents)
	}

	updatedEvents, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(updatedEvents) != 1 || updatedEvents[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("expected 1 EventBackgroundTaskTerminal from task_updated, got %+v", updatedEvents)
	}
}

// TestParser_CloseClearsState verifies Parser.Close empties the
// correlation maps so they don't leak across session teardown / restart
// within the same parser instance. Post-refactor the parser only
// holds backgroundToolUses, taskToolUses, streamBlockTypes and
// lastAssistantMessageID — no dedup sets.
func TestParser_CloseClearsState(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_started","task_id":"task-close","tool_use_id":"tool-close"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-close","patch":{"status":"completed"}}`)); err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	// Prime lastAssistantMessageID so we can see it clear.
	if _, err := parser.ParseLine(testThreadProto, []byte(`{"type":"assistant","message":{"id":"msg-pre","role":"assistant","content":[{"type":"text","text":"hi"}]}}`)); err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if parser.lastAssistantMessageID != "msg-pre" {
		t.Fatalf("precondition: lastAssistantMessageID = %q, want msg-pre", parser.lastAssistantMessageID)
	}

	parser.Close()

	if parser.lastAssistantMessageID != "" {
		t.Errorf("lastAssistantMessageID after Close: got %q, want empty", parser.lastAssistantMessageID)
	}
	if parser.backgroundToolUses != nil {
		t.Errorf("backgroundToolUses after Close: got %v, want nil", parser.backgroundToolUses)
	}
	if parser.taskToolUses != nil {
		t.Errorf("taskToolUses after Close: got %v, want nil", parser.taskToolUses)
	}
	if parser.streamBlockTypes != nil {
		t.Errorf("streamBlockTypes after Close: got %v, want nil", parser.streamBlockTypes)
	}

	// A fresh task_started after Close still emits normally.
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

// TestParseLine_ResultIsErrorTrueEmitsTurnComplete pins the
// post-cleanup behaviour: is_error=true on a result does not branch
// into EventError. The Python SDK's SDKResultError has no `error`
// (singular) field — only `errors: string[]` — so the legacy branch
// was pinning a phantom wire field. The result now settles through
// the normal EventTurnComplete path; interrupted detection uses
// errors[] via detectInterrupted.
func TestParseLine_ResultIsErrorTrueEmitsTurnComplete(t *testing.T) {
	line := []byte(`{"type":"result","is_error":true,"errors":["something failed"]}`)
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
