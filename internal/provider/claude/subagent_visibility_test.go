package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// Unit coverage for the agent-visibility parse rules whose failure mode
// is a DROP or a WRONG SCOPE rather than a wrong number — the cases a
// captured happy-path fixture cannot express.

// TestParseTaskProgress_UnresolvableToolUseIsDropped covers the
// reconnect case: a parser that came up mid-agent has no task binding,
// and a tick whose envelope also omits `tool_use_id` names nothing
// triage can update. It must be dropped, never emitted with an empty
// ItemID — an empty-ItemID event would be silently discarded by triage
// anyway, but only after crossing the whole pipeline.
func TestParseTaskProgress_UnresolvableToolUseIsDropped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_progress","task_id":"a-orphan",`+
			`"description":"Working","usage":{"total_tokens":10,"tool_uses":1,"duration_ms":5}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected the unattributable tick to be dropped, got %+v", events)
	}
}

// TestParseTaskProgress_ReSeedsTaskBinding pins the reconnect re-seed: a
// tick carrying BOTH ids restores the task_id ↔ tool_use_id mapping a
// restarted parser lost, so the LATER terminal (which carries only
// task_id) still resolves to the launch row.
func TestParseTaskProgress_ReSeedsTaskBinding(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_progress","task_id":"a-resumed","tool_use_id":"toolu_launch",`+
			`"description":"Working","usage":{"total_tokens":10,"tool_uses":1,"duration_ms":5}}`)); err != nil {
		t.Fatalf("parse progress: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_updated","task_id":"a-resumed","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("parse terminal: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("expected one terminal event, got %+v", events)
	}
	if events[0].ItemID != "toolu_launch" {
		t.Errorf("terminal ItemID = %q, want the re-seeded launch toolu_launch", events[0].ItemID)
	}
}

// TestParseTaskProgress_NestedAgentCarriesItsParent pins scope for a
// depth-2 agent: the tick's ParentToolUseID must be the LAUNCH's own
// parent (the depth-1 agent), which is what nests the child's card
// inside its parent's body.
func TestParseTaskProgress_NestedAgentCarriesItsParent(t *testing.T) {
	parser := NewParser()
	lines := []string{
		`{"type":"assistant","parent_tool_use_id":"toolu_depth1","message":{"id":"m","role":"assistant","content":[{"type":"tool_use","id":"toolu_depth2","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"a-child","tool_use_id":"toolu_depth2","task_type":"local_agent"}`,
	}
	for i, line := range lines {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_progress","task_id":"a-child","description":"Reading",`+
			`"usage":{"total_tokens":7,"tool_uses":1,"duration_ms":3}}`))
	if err != nil {
		t.Fatalf("parse progress: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 progress event, got %d", len(events))
	}
	if events[0].ItemID != "toolu_depth2" {
		t.Errorf("ItemID = %q, want toolu_depth2", events[0].ItemID)
	}
	if events[0].ParentToolUseID != "toolu_depth1" {
		t.Errorf("ParentToolUseID = %q, want toolu_depth1", events[0].ParentToolUseID)
	}
}

// TestParseBackgroundTasksChanged_AbsentVersusEmpty pins the one
// distinction that decides whether a live indicator can be wedged or
// wrongly cleared: `tasks: []` is a real empty set and must be
// forwarded; an ABSENT `tasks` key says nothing and must be dropped.
func TestParseBackgroundTasksChanged_AbsentVersusEmpty(t *testing.T) {
	parser := NewParser()

	absent, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"background_tasks_changed","uuid":"u1"}`))
	if err != nil {
		t.Fatalf("parse absent: %v", err)
	}
	if len(absent) != 0 {
		t.Fatalf("an absent tasks key must be dropped, got %+v", absent)
	}

	empty, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"background_tasks_changed","tasks":[],"uuid":"u2"}`))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(empty) != 1 {
		t.Fatalf("an empty tasks array is a real answer, got %+v", empty)
	}
	var meta provider.BackgroundTasksChangedMeta
	if err := json.Unmarshal(empty[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Tasks == nil || len(meta.Tasks) != 0 {
		t.Errorf("Tasks = %+v, want an allocated empty slice", meta.Tasks)
	}
	// Serialized as `[]`, never `null` — a consumer that swaps its set
	// for the payload must not be handed a nil it reads as "unknown".
	if !strings.Contains(string(empty[0].Meta), `"tasks":[]`) {
		t.Errorf("meta = %s, want tasks serialized as []", empty[0].Meta)
	}
}

// TestParseBackgroundTasksChanged_MalformedPayloadIsDropped guards the
// other direction: a `tasks` value we cannot read is not evidence that
// nothing is running, so it must be dropped like an absent key rather
// than applied as an empty set.
func TestParseBackgroundTasksChanged_MalformedPayloadIsDropped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"background_tasks_changed","tasks":"everything"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a malformed tasks payload must be dropped, got %+v", events)
	}
}

// TestParseTaskUpdated_BackgroundedPatchKeepsLiveAgentTask is the
// companion of TestAppendToolResultBlock_NonTerminalUpdateKeepsLiveAgentTask
// (commit 230cd078) for the shape that arm now RECOGNISES: an
// `is_backgrounded` patch emits its own event, and must still leave
// signal (5) armed so the §E5 ack that follows ~40ms later classifies
// as a launch ack rather than the agent's real result.
func TestParseTaskUpdated_BackgroundedPatchKeepsLiveAgentTask(t *testing.T) {
	parser := NewParser()
	setup := []string{
		`{"type":"assistant","message":{"id":"m","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"a-live","tool_use_id":"toolu_launch","task_type":"local_agent"}`,
	}
	for i, line := range setup {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatalf("setup line %d: %v", i, err)
		}
	}

	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_updated","task_id":"a-live","patch":{"is_backgrounded":true}}`))
	if err != nil {
		t.Fatalf("parse backgrounded patch: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventSubagentBackgrounded {
		t.Fatalf("expected one subagent_backgrounded event, got %+v", events)
	}
	if events[0].ItemID != "toolu_launch" {
		t.Errorf("ItemID = %q, want toolu_launch", events[0].ItemID)
	}
	if !parser.hasLiveAgentTask("toolu_launch") {
		t.Fatal("the backgrounded patch must not disarm signal (5)")
	}

	ack, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_launch","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]}}`))
	if err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	if len(ack) != 1 {
		t.Fatalf("expected 1 ack event, got %d", len(ack))
	}
	var meta map[string]any
	if err := json.Unmarshal(ack[0].Meta, &meta); err != nil {
		t.Fatalf("decode ack meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Errorf("ack lost is_background after a backgrounded patch: %v", meta)
	}
}

// TestParseTaskUpdated_TerminalStillWins pins that adding the
// backgrounded branch did not steal the terminal path: a patch carrying
// BOTH a terminal status and is_backgrounded is a terminal.
func TestParseTaskUpdated_TerminalStillWins(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_started","task_id":"a-done","tool_use_id":"toolu_launch","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_updated","task_id":"a-done","patch":{"is_backgrounded":true,"status":"completed"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("expected the terminal to win, got %+v", events)
	}
}

// TestParseTaskUpdated_BackgroundedWithoutBindingIsDropped: the event's
// entire payload is the launch it belongs to, so an unattributable
// patch is dropped rather than emitted with an empty ItemID.
func TestParseTaskUpdated_BackgroundedWithoutBindingIsDropped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_updated","task_id":"a-orphan","patch":{"is_backgrounded":true}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected the unattributable patch to be dropped, got %+v", events)
	}
}

// TestParseTaskUpdated_RunningPatchStaysSilent pins that the new branch
// did not turn ordinary progress patches into events.
func TestParseTaskUpdated_RunningPatchStaysSilent(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_started","task_id":"a-run","tool_use_id":"toolu_launch","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_updated","task_id":"a-run","patch":{"status":"running"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a running patch must stay a no-op, got %+v", events)
	}
}

// TestParseControlRequest_UnresolvableAgentIDLeavesScopeEmpty: guessing
// a scope would nest an approval under the wrong card. Triage owns the
// row-lookup fallback, so the parser must leave the field empty.
func TestParseControlRequest_UnresolvableAgentIDLeavesScopeEmpty(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash",`+
			`"input":{"command":"ls"},"tool_use_id":"toolu_asked","agent_id":"a-unknown"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 approval event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "" {
		t.Errorf("ParentToolUseID = %q, want empty for an unresolvable agent_id", events[0].ParentToolUseID)
	}
}

// TestParseControlRequest_MainAgentApprovalHasNoScope: the main agent's
// ask carries no agent_id at all and must stay top-level.
func TestParseControlRequest_MainAgentApprovalHasNoScope(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash",`+
			`"input":{"command":"ls"},"tool_use_id":"toolu_asked"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].ParentToolUseID != "" {
		t.Fatalf("main-agent approval must be unscoped, got %+v", events)
	}
	var req provider.ApprovalRequest
	if err := json.Unmarshal(events[0].Meta, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.ParentToolUseID != "" {
		t.Errorf("ApprovalRequest.ParentToolUseID = %q, want empty", req.ParentToolUseID)
	}
}

// TestParseControlRequest_AskUserQuestionCarriesAgentScope: the
// structured user-input path is the second half of the approval scoping
// rule — an AskUserQuestion a subagent raises must nest under the same
// card its tool approvals do.
func TestParseControlRequest_AskUserQuestionCarriesAgentScope(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_started","task_id":"a-ask","tool_use_id":"toolu_launch","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion",`+
			`"input":{"questions":[{"question":"Which lane?","header":"Lane","options":[{"label":"A"},{"label":"B"}]}]},`+
			`"tool_use_id":"toolu_ask","agent_id":"a-ask"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventUserInputRequest {
		t.Fatalf("expected a user input request, got %+v", events)
	}
	if events[0].ParentToolUseID != "toolu_launch" {
		t.Errorf("event ParentToolUseID = %q, want toolu_launch", events[0].ParentToolUseID)
	}
	var req provider.UserInputRequest
	if err := json.Unmarshal(events[0].Meta, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.ParentToolUseID != "toolu_launch" {
		t.Errorf("UserInputRequest.ParentToolUseID = %q, want toolu_launch", req.ParentToolUseID)
	}
}

// TestAppendToolResultBlock_InlineSkillGetsNoForkStamp: a skill that did
// NOT fork answers `{success, commandName}` with no status. Stamping it
// would turn every inline skill into a phantom agent node.
func TestAppendToolResultBlock_InlineSkillGetsNoForkStamp(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_skill","type":"tool_result",`+
			`"content":[{"type":"text","text":"Launching skill: handoff"}]}]},`+
			`"tool_use_result":{"success":true,"commandName":"handoff"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 completion, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, stamped := meta["skillFork"]; stamped {
		t.Errorf("an inline skill must not carry skillFork: %s", events[0].Meta)
	}
}

// TestAppendToolResultBlock_ForkedSkillWithoutAgentIDIsNotStamped: the
// id is the whole point of the stamp (it is what a later approval
// addresses the fork by), so a `forked` result with no agentId is left
// unstamped rather than stamped empty.
func TestAppendToolResultBlock_ForkedSkillWithoutAgentIDIsNotStamped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_skill","type":"tool_result",`+
			`"content":[{"type":"text","text":"Skill \"x\" completed (forked execution)."}]}]},`+
			`"tool_use_result":{"success":true,"commandName":"x","status":"forked"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 completion, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, stamped := meta["skillFork"]; stamped {
		t.Errorf("an agentId-less forked result must not be stamped: %s", events[0].Meta)
	}
}

// TestAppendToolResultBlock_InlineAgentCompletionIsNotAFork guards the
// discriminator: an inline agent's real completion carries `agentId`
// and a `status`, but that status is `completed` — never `forked`.
func TestAppendToolResultBlock_InlineAgentCompletionIsNotAFork(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_agent","type":"tool_result",`+
			`"content":[{"type":"text","text":"done"}]}]},`+
			`"tool_use_result":{"agentId":"a1","agentType":"general-purpose","status":"completed","totalTokens":10}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, stamped := meta["skillFork"]; stamped {
		t.Errorf("an inline agent completion must not be stamped as a fork: %s", events[0].Meta)
	}
}

// TestBuildBackgroundTaskNotification_NoUsageLeavesMetaClean: a
// `local_bash` bookend carries no usage at all, and stamping a zeroed
// object would overwrite the agent's real final numbers in triage's
// merge.
func TestBuildBackgroundTaskNotification_NoUsageLeavesMetaClean(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"task_notification","task_id":"b-bash","tool_use_id":"toolu_bash",`+
			`"status":"completed","summary":"Sleep"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, present := meta["usage"]; present {
		t.Errorf("a usage-less notification must not stamp a usage block: %s", events[0].Meta)
	}
}
