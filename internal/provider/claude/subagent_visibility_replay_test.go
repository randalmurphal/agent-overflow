package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// Replay coverage for the agent-visibility wire surface
// (docs/specs/agent-visibility.md): the live `system/task_progress`
// tick, the `system/background_tasks_changed` level set, the
// `background_tasks` control round-trip's non-terminal
// `patch.is_backgrounded`, a subagent's `can_use_tool` carrying
// `agent_id`, and the forked-skill completion (§E9).
//
// Every fixture below is a real 2.1.237 capture — the first three from
// the 2026-08-22 steering spike, the fourth lifted out of an AO
// provider-events log — so a drift in any of these shapes fails here
// rather than silently degrading a card to "no progress ever arrives".
const (
	fixtureTaskProgress          = "../../../docs/references/fixtures/claude/task_progress_20260822.ndjson"
	fixtureBackgroundTasksCtl    = "../../../docs/references/fixtures/claude/background_tasks_control_20260822.ndjson"
	fixtureCanUseToolAgentID     = "../../../docs/references/fixtures/claude/can_use_tool_agent_id_20260822.ndjson"
	fixtureForkedSkill           = "../../../docs/references/fixtures/claude/forked_skill_20260822.ndjson"
	fixtureProgressLaunchToolUse = "toolu_01LQR6oybvv33HgSQ56rxSMN"
	fixtureProgressTaskID        = "a75a87231e749bff1"
)

// decodeProgress unmarshals an EventSubagentProgress meta, failing the
// test on a shape triage could not read either.
func decodeProgress(t *testing.T, evt provider.ProviderEvent) provider.SubagentProgressMeta {
	t.Helper()
	var meta provider.SubagentProgressMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode subagent progress meta: %v (%s)", err, evt.Meta)
	}
	return meta
}

func decodeBackgroundSet(t *testing.T, evt provider.ProviderEvent) provider.BackgroundTasksChangedMeta {
	t.Helper()
	var meta provider.BackgroundTasksChangedMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode background tasks changed meta: %v (%s)", err, evt.Meta)
	}
	return meta
}

func decodeMetaMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode meta: %v (%s)", err, raw)
	}
	return meta
}

// TestReplay_TaskProgressFixture pins the whole live-progress surface of
// a real `local_agent` run: two ticks, both scoped to the LAUNCH
// tool_use (not the agent's own Bash), carrying the cumulative counters
// a card renders, and the completing `task_notification` carrying the
// authoritative final `usage` under the contract key triage decodes.
func TestReplay_TaskProgressFixture(t *testing.T) {
	events := replayFixture(t, fixtureTaskProgress)

	ticks := filterKinds(events, provider.EventSubagentProgress)
	if len(ticks) != 2 {
		t.Fatalf("expected 2 task_progress ticks, got %d", len(ticks))
	}
	wantTicks := []provider.SubagentProgressMeta{
		{TaskID: fixtureProgressTaskID, ToolUses: 1, TotalTokens: 18227, DurationMs: 2368,
			Activity: "Running Sleep for 40 seconds", LastToolName: "Bash", AgentType: "general-purpose"},
		{TaskID: fixtureProgressTaskID, ToolUses: 2, TotalTokens: 19690, DurationMs: 4664,
			Activity: "Running Sleep for 40 seconds", LastToolName: "Bash", AgentType: "general-purpose"},
	}
	for i, tick := range ticks {
		if tick.ItemID != fixtureProgressLaunchToolUse {
			t.Errorf("tick %d ItemID = %q, want the launch tool_use %q", i, tick.ItemID, fixtureProgressLaunchToolUse)
		}
		if tick.ParentToolUseID != "" {
			t.Errorf("tick %d ParentToolUseID = %q, want empty (top-level launch)", i, tick.ParentToolUseID)
		}
		if got := decodeProgress(t, tick); got != wantTicks[i] {
			t.Errorf("tick %d meta = %+v, want %+v", i, got, wantTicks[i])
		}
	}

	// The agent's completing notification is the one envelope carrying
	// the run's final counters. Triage folds these onto the launch row,
	// so the key must stay `usage` and the type SubagentProgressMeta.
	notifications := filterKinds(events, provider.EventBackgroundTaskNotification)
	var agentNotification *provider.ProviderEvent
	for i := range notifications {
		if decodeMetaMap(t, notifications[i].Meta)["task_id"] == fixtureProgressTaskID {
			agentNotification = &notifications[i]
		}
	}
	if agentNotification == nil {
		t.Fatalf("no task_notification for the agent's task_id %s", fixtureProgressTaskID)
	}
	var decoded struct {
		Usage provider.SubagentProgressMeta `json:"usage"`
	}
	if err := json.Unmarshal(agentNotification.Meta, &decoded); err != nil {
		t.Fatalf("decode notification meta: %v", err)
	}
	want := provider.SubagentProgressMeta{ToolUses: 2, TotalTokens: 20055, DurationMs: 6883}
	if decoded.Usage != want {
		t.Errorf("notification usage = %+v, want %+v", decoded.Usage, want)
	}

	// The nested backgrounded Bash the agent launched moves the level
	// set twice: one member, then empty. The empty frame is a real
	// answer and must survive as an allocated slice, not a nil.
	sets := filterKinds(events, provider.EventBackgroundTasksChanged)
	if len(sets) != 2 {
		t.Fatalf("expected 2 background_tasks_changed frames, got %d", len(sets))
	}
	first := decodeBackgroundSet(t, sets[0])
	if len(first.Tasks) != 1 || first.Tasks[0].TaskID != "b8tm4jomt" || first.Tasks[0].TaskType != "local_bash" {
		t.Errorf("first level set = %+v, want the single local_bash task", first.Tasks)
	}
	second := decodeBackgroundSet(t, sets[1])
	if second.Tasks == nil || len(second.Tasks) != 0 {
		t.Errorf("second level set = %+v, want an allocated empty slice", second.Tasks)
	}

	// A foreground agent is never "backgrounded mid-flight".
	if backgrounded := filterKinds(events, provider.EventSubagentBackgrounded); len(backgrounded) != 0 {
		t.Errorf("expected no subagent_backgrounded events, got %d", len(backgrounded))
	}
}

// TestReplay_BackgroundTasksControlFixture pins the reply half of AO's
// own `background_tasks` control_request: the CLI's non-terminal
// `patch:{is_backgrounded:true}` must become EventSubagentBackgrounded
// on the LAUNCH row, must NOT become a terminal, and the level set that
// rides with it must resolve the task to the same launch tool_use.
func TestReplay_BackgroundTasksControlFixture(t *testing.T) {
	const (
		launch = "toolu_016U5CANM15pvGuV83L4Getr"
		taskID = "a8ba85b6e43fcb61d"
	)
	events := replayFixture(t, fixtureBackgroundTasksCtl)

	backgrounded := filterKinds(events, provider.EventSubagentBackgrounded)
	if len(backgrounded) != 1 {
		t.Fatalf("expected 1 subagent_backgrounded event, got %d", len(backgrounded))
	}
	if backgrounded[0].ItemID != launch {
		t.Errorf("ItemID = %q, want the launch tool_use %q", backgrounded[0].ItemID, launch)
	}
	var meta provider.SubagentBackgroundedMeta
	if err := json.Unmarshal(backgrounded[0].Meta, &meta); err != nil {
		t.Fatalf("decode backgrounded meta: %v", err)
	}
	if meta.TaskID != taskID {
		t.Errorf("meta.TaskID = %q, want %q", meta.TaskID, taskID)
	}

	// `is_backgrounded` is NOT a terminal — a terminal here would settle
	// the launch row while the agent is still working.
	if terminals := filterKinds(events, provider.EventBackgroundTaskTerminal); len(terminals) != 0 {
		t.Errorf("expected no background task terminal, got %d: %+v", len(terminals), terminals)
	}

	sets := filterKinds(events, provider.EventBackgroundTasksChanged)
	if len(sets) != 1 {
		t.Fatalf("expected 1 background_tasks_changed frame, got %d", len(sets))
	}
	set := decodeBackgroundSet(t, sets[0])
	if len(set.Tasks) != 1 {
		t.Fatalf("level set = %+v, want one member", set.Tasks)
	}
	if set.Tasks[0].TaskID != taskID || set.Tasks[0].ToolUseID != launch {
		t.Errorf("level set member = %+v, want task %s resolved to launch %s", set.Tasks[0], taskID, launch)
	}
	if set.Tasks[0].TaskType != "local_agent" || set.Tasks[0].Description != "spike2" {
		t.Errorf("level set member lost its descriptive fields: %+v", set.Tasks[0])
	}

	// The §E5 async ack that follows still classifies as a background
	// launch — the backgrounded patch must not have disarmed liveness.
	var ack *provider.ProviderEvent
	for i, evt := range events {
		if evt.Kind == provider.EventToolComplete && evt.ItemID == launch {
			ack = &events[i]
		}
	}
	if ack == nil {
		t.Fatal("no EventToolComplete for the launch tool_use")
	}
	if decodeMetaMap(t, ack.Meta)["is_background"] != true {
		t.Errorf("ack meta lost is_background: %s", ack.Meta)
	}
}

// TestReplay_CanUseToolAgentIDFixture pins subagent approval scoping: a
// subagent's `can_use_tool` carries `agent_id` (its task id) and NO
// parent_tool_use_id anywhere on the envelope, so the only route to the
// agent's card is the parser's task map.
func TestReplay_CanUseToolAgentIDFixture(t *testing.T) {
	const (
		launch    = "toolu_01SXeiUJutAjUJjzeriwvpQD"
		askedTool = "toolu_01WNshzNu1eaLLYXZVXjLAt8"
	)
	events := replayFixture(t, fixtureCanUseToolAgentID)

	approvals := filterKinds(events, provider.EventApprovalRequest)
	if len(approvals) != 1 {
		t.Fatalf("expected 1 approval request, got %d", len(approvals))
	}
	if approvals[0].ParentToolUseID != launch {
		t.Errorf("event ParentToolUseID = %q, want the launch tool_use %q", approvals[0].ParentToolUseID, launch)
	}
	var req provider.ApprovalRequest
	if err := json.Unmarshal(approvals[0].Meta, &req); err != nil {
		t.Fatalf("decode approval request: %v", err)
	}
	if req.ParentToolUseID != launch {
		t.Errorf("ApprovalRequest.ParentToolUseID = %q, want %q", req.ParentToolUseID, launch)
	}
	if req.ToolUseID != askedTool {
		t.Errorf("ApprovalRequest.ToolUseID = %q, want the asked tool %q", req.ToolUseID, askedTool)
	}
	if req.ToolName != "Write" {
		t.Errorf("ApprovalRequest.ToolName = %q, want Write", req.ToolName)
	}
}

// TestReplay_ForkedSkillFixture pins §E9: a forked skill has NO task
// lifecycle at all, its rows are attributed to the Skill tool_use, and
// the completion's `status:"forked"` is the only identity statement —
// which must both stamp the launch row and bind the fork's agentId so a
// later approval resolves to it.
func TestReplay_ForkedSkillFixture(t *testing.T) {
	const (
		skillToolUse = "toolu_01NUxYMHJ5muvDqv4V1RfZqW"
		forkAgentID  = "ababe998963e4e82a"
	)
	parser := NewParser()
	var events []provider.ProviderEvent
	for i, line := range loadNDJSONFixture(t, fixtureForkedSkill) {
		got, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d: parse error: %v", i+1, err)
		}
		events = append(events, got...)
	}

	// A fork produces none of the local_agent task signals.
	for _, kind := range []provider.EventKind{
		provider.EventSubagentProgress,
		provider.EventBackgroundTasksChanged,
		provider.EventSubagentBackgrounded,
		provider.EventBackgroundTaskTerminal,
	} {
		if got := filterKinds(events, kind); len(got) != 0 {
			t.Errorf("a forked skill must emit no %s events, got %d", kind, len(got))
		}
	}

	var completion *provider.ProviderEvent
	for i, evt := range events {
		if evt.Kind == provider.EventToolComplete && evt.ItemID == skillToolUse {
			completion = &events[i]
		}
	}
	if completion == nil {
		t.Fatal("no EventToolComplete for the Skill tool_use")
	}
	meta := decodeMetaMap(t, completion.Meta)
	fork, ok := meta["skillFork"].(map[string]any)
	if !ok {
		t.Fatalf("Skill completion meta has no skillFork block: %s", completion.Meta)
	}
	if fork["agentId"] != forkAgentID {
		t.Errorf("skillFork.agentId = %v, want %q", fork["agentId"], forkAgentID)
	}
	if fork["commandName"] != "code-review" {
		t.Errorf("skillFork.commandName = %v, want code-review", fork["commandName"])
	}
	// The completion must not be marked background: the main turn was
	// blocked for the whole fork, and a background row would keep the
	// launch at status=running forever with no terminal ever coming.
	if _, marked := meta["is_background"]; marked {
		t.Errorf("a forked skill completion must not be is_background: %s", completion.Meta)
	}

	// Attribution: the fork's rows carry the Skill tool_use as parent.
	var attributed int
	for _, evt := range events {
		if evt.ParentToolUseID == skillToolUse {
			attributed++
		}
	}
	if attributed == 0 {
		t.Error("expected the fork's sidechain rows to carry the Skill tool_use as ParentToolUseID")
	}

	// The stamp also binds agentId → the Skill tool_use, so a
	// subsequent subagent approval resolves to the fork's card.
	approval, err := parser.ParseLine(testThread, []byte(
		`{"type":"control_request","request_id":"req-fork","request":{"subtype":"can_use_tool","tool_name":"Bash",`+
			`"input":{"command":"ls"},"tool_use_id":"toolu_fork_child","agent_id":"`+forkAgentID+`"}}`))
	if err != nil {
		t.Fatalf("parse fork approval: %v", err)
	}
	if len(approval) != 1 {
		t.Fatalf("expected 1 approval event, got %d", len(approval))
	}
	if approval[0].ParentToolUseID != skillToolUse {
		t.Errorf("fork approval ParentToolUseID = %q, want the Skill tool_use %q", approval[0].ParentToolUseID, skillToolUse)
	}
}
