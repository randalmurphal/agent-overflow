package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestTranscriptMirrorTurnsDirectForkedCommandIntoLiveSkill(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	started := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	if len(started) != 2 || started[0].Kind != provider.EventToolStart || started[0].ItemType != "Command" || started[1].Kind != provider.EventCommandLifecycle {
		t.Fatalf("command start = %+v, want provisional command + lifecycle", started)
	}
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`); len(events) != 0 {
		t.Fatalf("fork metadata emitted before classification: %+v", events)
	}

	// The prompt can be mirrored before the first attributed assistant row.
	// The row is already visible. Retain the prompt, then change that same
	// row to Skill when sidechain attribution proves this file is the fork root.
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"user","uuid":"u-root","agentId":"root","timestamp":"2026-08-24T12:00:00Z","message":{"role":"user","content":"review the change"}}]}`); len(events) != 0 {
		t.Fatalf("unattributed mirror emitted beyond the existing command row: %+v", events)
	}
	events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-tool","agentId":"root","isSidechain":true,"attributionSkill":"code-review","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`)
	if len(events) != 3 {
		t.Fatalf("attributed mirror batch emitted %d events, want launch + prompt + tool: %+v", len(events), events)
	}
	launchID := "claude-command:cmd-1"
	if events[0].Kind != provider.EventToolStart || events[0].ItemID != launchID || events[0].ItemType != "Skill" {
		t.Fatalf("direct command launch = %+v", events[0])
	}
	var launchMeta map[string]any
	if err := json.Unmarshal(events[0].Meta, &launchMeta); err != nil {
		t.Fatalf("decode launch meta: %v", err)
	}
	if _, ok := launchMeta["skillFork"].(map[string]any); !ok {
		t.Fatalf("live Skill launch has no durable fork proof: %s", events[0].Meta)
	}
	if launchMeta["directCommandFork"] != true {
		t.Fatalf("live Skill launch has no direct-command marker: %s", events[0].Meta)
	}
	if events[1].Kind != provider.EventUserText || events[1].ParentToolUseID != launchID {
		t.Fatalf("mirrored prompt = %+v", events[1])
	}
	if events[2].Kind != provider.EventToolStart || events[2].ParentToolUseID != launchID || events[2].ItemID != "toolu-read" {
		t.Fatalf("mirrored tool = %+v", events[2])
	}

	// Claude's ordinary command result is only the outer synthetic wrapper.
	// Once the mirror proves a fork, the wrapper stays hidden.
	if events := parse(`{"type":"assistant","message":{"id":"synthetic-1","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"review complete"}]}}`); len(events) != 0 {
		t.Fatalf("synthetic fork wrapper leaked: %+v", events)
	}
	if events := parse(`{"type":"assistant","message":{"id":"synthetic-2","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"second page"}]}}`); len(events) != 0 {
		t.Fatalf("second synthetic fork wrapper leaked: %+v", events)
	}
	final := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-final","agentId":"root","isSidechain":true,"attributionSkill":"code-review","timestamp":"2026-08-24T12:00:02Z","message":{"id":"msg-final","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"No findings."}]}}]}`)
	if len(final) != 1 || final[0].Kind != provider.EventTextDelta || final[0].ParentToolUseID != launchID {
		t.Fatalf("mirrored final = %+v", final)
	}

	result := parse(`{"type":"result","subtype":"success","is_error":false,"result":"review complete"}`)
	if len(result) != 3 || result[0].Kind != provider.EventToolComplete || result[0].ItemID != launchID || result[1].Kind != provider.EventCommandResult || result[2].Kind != provider.EventTurnComplete {
		t.Fatalf("result events = %+v", result)
	}
	if result[1].Content != "review complete\n\nsecond page" || result[1].ParentToolUseID != "" {
		t.Fatalf("fork result = %+v, want one combined top-level synthetic answer", result[1])
	}
	var resultMeta provider.CommandResultMeta
	if err := json.Unmarshal(result[1].Meta, &resultMeta); err != nil {
		t.Fatalf("decode result meta: %v", err)
	}
	if resultMeta.AgentResult == nil || resultMeta.AgentResult.LaunchID != launchID || resultMeta.AgentResult.SourceKind != "skill" || resultMeta.AgentResult.SourceName != "code-review" {
		t.Fatalf("fork result source = %+v", resultMeta.AgentResult)
	}
	var meta map[string]any
	if err := json.Unmarshal(result[0].Meta, &meta); err != nil {
		t.Fatalf("decode completion meta: %v", err)
	}
	if _, ok := meta["skillFork"].(map[string]any); !ok {
		t.Fatalf("completion has no skillFork marker: %s", result[0].Meta)
	}
	if meta["directCommandResult"] != true {
		t.Fatalf("completion has no direct-command result marker: %s", result[0].Meta)
	}
	terminal := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"completed"}`)
	if len(terminal) != 1 || terminal[0].Kind != provider.EventCommandLifecycle {
		t.Fatalf("terminal events = %+v, want bookkeeping only after result settled the command", terminal)
	}
	if len(parser.transcriptMirror.projections) != 0 || len(parser.transcriptMirror.taskScopes) != 0 || len(parser.transcriptMirror.scopeOwners) != 0 {
		t.Fatalf("completed command retained mirror state: %+v", parser.transcriptMirror)
	}
}

func TestTranscriptMirrorDoesNotTurnAttributedMainWorkIntoSkill(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/brainstorm", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	started := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	if len(started) != 2 || started[0].Kind != provider.EventToolStart || started[0].ItemType != "Command" {
		t.Fatalf("command start = %+v", started)
	}
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/session.jsonl","entries":[{"type":"user","uuid":"u-root","attributionSkill":"brainstorm","timestamp":"2026-08-24T12:00:00Z","message":{"role":"user","content":"brainstorm this"}}]}`); len(events) != 0 {
		t.Fatalf("scope-unknown transcript emitted events: %+v", events)
	}
	if got := len(parser.transcriptMirror.pending["/tmp/session.jsonl"]); got != 1 {
		t.Fatalf("scope-unknown transcript buffered %d rows, want 1", got)
	}

	events := parse(`{"type":"transcript_mirror","filePath":"/tmp/session.jsonl","entries":[{"type":"assistant","uuid":"a-agent","isSidechain":false,"attributionSkill":"brainstorm","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-agent","name":"Agent","input":{"description":"Survey implementation","prompt":"inspect"}}]}}]}`)
	if len(events) != 0 {
		t.Fatalf("attributed main transcript was projected as a fork: %+v", events)
	}
	command := parser.transcriptMirror.commands["cmd-1"]
	if command == nil || command.forked {
		t.Fatalf("main-transcript command classified as forked: %+v", command)
	}
	if len(parser.transcriptMirror.projections) != 0 {
		t.Fatalf("main transcript opened sidechain projections: %+v", parser.transcriptMirror.projections)
	}
	if len(parser.transcriptMirror.pending) != 0 || parser.transcriptMirror.totalPendingBytes != 0 {
		t.Fatalf("classified main transcript retained pending data: %+v", parser.transcriptMirror.pending)
	}
}

func TestTranscriptMirrorAgentMetadataWinsRaceWithTaskStarted(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/brainstorm", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	parent := parse(`{"type":"assistant","message":{"id":"msg-parent","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-agent","name":"Agent","input":{"description":"Survey implementation","prompt":"inspect","run_in_background":true}}]}}`)
	if len(parent) != 1 || parent[0].Kind != provider.EventToolStart || parent[0].ItemID != "toolu-agent" {
		t.Fatalf("parent Agent launch = %+v", parent)
	}
	parse(`{"type":"system","subtype":"task_started","task_id":"child-agent","tool_use_id":"toolu-agent","task_type":"local_agent"}`)
	if binding := parser.transcriptMirror.taskScopes["child-agent"]; binding.scope != "" {
		t.Fatalf("task_started unexpectedly owned an unmirrored launch: %+v", binding)
	}
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose","description":"Survey implementation","toolUseId":"toolu-agent","spawnDepth":1}]}`); len(events) != 0 {
		t.Fatalf("ordinary Agent metadata opened a duplicate mirror: %+v", events)
	}

	events := parse(`{"type":"assistant","parent_tool_use_id":"toolu-agent","message":{"id":"msg-child","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}`)
	if len(events) == 0 {
		t.Fatal("ordinary child stdout emitted no events")
	}
	childTool := events[len(events)-1]
	if childTool.Kind != provider.EventToolStart || childTool.ItemID != "toolu-read" || childTool.ParentToolUseID != "toolu-agent" {
		t.Fatalf("ordinary child stdout = %+v, want child tool beneath Agent launch", events)
	}
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"assistant","uuid":"a-child","agentId":"child-agent","isSidechain":true,"attributionSkill":"brainstorm","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-child","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`); len(events) != 0 {
		t.Fatalf("ordinary Agent mirror duplicated stdout: %+v", events)
	}
	command := parser.transcriptMirror.commands["cmd-1"]
	if command == nil || command.forked {
		t.Fatalf("inline command classified as forked Skill: %+v", command)
	}
	if projection := parser.transcriptMirror.projections["/tmp/agent-child-agent.jsonl"]; projection != nil {
		t.Fatalf("ordinary Agent opened mirror projection: %+v", projection)
	}
	parser.noteMirrorTaskScope("child-agent", "toolu-agent", true)
	if binding := parser.transcriptMirror.taskScopes["child-agent"]; binding.needsProjection {
		t.Fatalf("later task_started promoted duplicate mirror: %+v", binding)
	}
	parser.finishMirroredTask(testThread, "child-agent")
	if _, exists := parser.transcriptMirror.taskScopes["child-agent"]; exists {
		t.Fatal("terminal ordinary task retained mirror classification")
	}
}

func TestTranscriptMirrorWaitsForForkMetadata(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	if events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-root","agentId":"root","isSidechain":true,"attributionSkill":"code-review","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"reviewing"}]}}]}`); len(events) != 0 {
		t.Fatalf("unproven fork emitted events: %+v", events)
	}
	if parser.transcriptMirror.commands["cmd-1"].forked {
		t.Fatal("sidechain attribution alone classified the command as a Skill")
	}

	events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`)
	if len(events) != 2 || events[0].Kind != provider.EventToolStart || events[0].ItemType != "Skill" || events[1].Kind != provider.EventTextDelta {
		t.Fatalf("confirmed fork events = %+v", events)
	}
}

func TestTranscriptMirrorRejectsChangingAgentMetadataOwner(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/brainstorm", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	parse := func(line string) ([]provider.ProviderEvent, error) {
		return parser.ParseLine(testThread, []byte(line))
	}

	if _, err := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`); err != nil {
		t.Fatalf("start command: %v", err)
	}
	if _, err := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"agent_metadata","toolUseId":"toolu-agent"}]}`); err != nil {
		t.Fatalf("initial metadata: %v", err)
	}
	_, err := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"agent_metadata","toolUseId":"toolu-other"}]}`)
	if err == nil || !strings.Contains(err.Error(), "changed agent_metadata toolUseId") {
		t.Fatalf("changed metadata owner returned %v", err)
	}
}

func TestInspectMirrorEntriesRejectsMixedTranscriptScopes(t *testing.T) {
	_, err := inspectMirrorEntries([]json.RawMessage{
		json.RawMessage(`{"uuid":"main","isSidechain":false}`),
		json.RawMessage(`{"uuid":"child","isSidechain":true}`),
	})
	if err == nil || !strings.Contains(err.Error(), "mixes main-transcript and sidechain") {
		t.Fatalf("mixed transcript scopes returned %v", err)
	}
}

func TestInspectMirrorEntriesRejectsConflictingAgentMetadataOwners(t *testing.T) {
	_, err := inspectMirrorEntries([]json.RawMessage{
		json.RawMessage(`{"type":"agent_metadata","toolUseId":"toolu-one"}`),
		json.RawMessage(`{"type":"agent_metadata","toolUseId":"toolu-two"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting agent_metadata") {
		t.Fatalf("conflicting metadata owners returned %v", err)
	}
}

func TestTranscriptMirrorSyntheticAnswerBeatsCancelledLifecycleForStatus(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		return events
	}
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`)
	parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-tool","agentId":"root","isSidechain":true,"attributionSkill":"code-review","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"reviewed"}]}}]}`)
	parse(`{"type":"assistant","message":{"id":"synthetic-1","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"final findings"}]}}`)
	events := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"cancelled"}`)
	if len(events) < 2 || events[0].Kind != provider.EventToolComplete || events[1].Kind != provider.EventCommandResult {
		t.Fatalf("terminal events = %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode completion meta: %v", err)
	}
	if meta["is_error"] == true {
		t.Fatalf("synthetic answer rendered as failed: %s", events[0].Meta)
	}
}

func TestTranscriptMirrorEmptySyntheticWrapperDoesNotHideCancelledStatus(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		return events
	}
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`)
	parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-tool","agentId":"root","isSidechain":true,"attributionSkill":"code-review","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"reviewed"}]}}]}`)
	parse(`{"type":"assistant","message":{"id":"synthetic-1","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":""}]}}`)
	events := parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"cancelled"}`)
	if len(events) == 0 || events[0].Kind != provider.EventToolComplete {
		t.Fatalf("terminal events = %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode completion meta: %v", err)
	}
	if meta["is_error"] != true {
		t.Fatalf("empty wrapper hid cancelled status: %s", events[0].Meta)
	}
}

func TestTranscriptMirrorNestedProjectionInheritsCommandAndIsReleased(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`)
	root := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-root.jsonl","entries":[{"type":"assistant","uuid":"a-launch","agentId":"root","isSidechain":true,"attributionSkill":"code-review","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-root","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-child","name":"Agent","input":{"description":"nested review","prompt":"inspect"}}]}},{"type":"user","uuid":"u-child-result","agentId":"root","isSidechain":true,"timestamp":"2026-08-24T12:00:02Z","toolUseResult":{"agentId":"child-agent"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-child","content":"spawned"}]}}]}`)
	if len(root) < 2 {
		t.Fatalf("root mirror did not emit nested launch: %+v", root)
	}
	if binding := parser.transcriptMirror.taskScopes["child-agent"]; binding.scope != "toolu-child" {
		t.Fatalf("nested task binding = %+v", binding)
	}

	child := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"assistant","uuid":"a-child","agentId":"child-agent","isSidechain":true,"timestamp":"2026-08-24T12:00:03Z","message":{"id":"msg-child","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`)
	if len(child) < 2 {
		t.Fatalf("child mirror was not projected: %+v", child)
	}
	projection := parser.transcriptMirror.projections["/tmp/agent-child-agent.jsonl"]
	if projection == nil || projection.commandUUID != "cmd-1" {
		t.Fatalf("nested projection did not inherit root command: %+v", projection)
	}
	parser.noteMirrorTaskScope("child-agent", "toolu-child", true)
	if binding := parser.transcriptMirror.taskScopes["child-agent"]; binding.projectionKey != "/tmp/agent-child-agent.jsonl" || !binding.needsProjection {
		t.Fatalf("later task_started discarded nested projection binding: %+v", binding)
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"completed"}`)
	state := parser.transcriptMirror
	if len(state.projections) != 0 || len(state.taskScopes) != 0 || len(state.scopeOwners) != 0 {
		t.Fatalf("completed nested command retained mirror state: %+v", state)
	}
}

func TestTranscriptMirrorProjectsNestedBatchThatArrivesAfterTaskTerminal(t *testing.T) {
	parser := NewParser()
	state := parser.ensureTranscriptMirrorState()
	if _, err := state.newProjection("/tmp/agent-root.jsonl", "root-launch", "root", "cmd-1"); err != nil {
		t.Fatalf("new root projection: %v", err)
	}
	state.scopeOwners["toolu-child"] = "/tmp/agent-root.jsonl"
	state.taskScopes["child-agent"] = mirrorTaskScope{scope: "toolu-child", needsProjection: true}

	if events := parser.finishMirroredTask(testThread, "child-agent"); len(events) != 0 {
		t.Fatalf("terminal before projection emitted events: %+v", events)
	}
	if !state.taskScopes["child-agent"].terminal {
		t.Fatal("terminal-before-mirror binding was discarded")
	}

	events, err := parser.ParseLine(testThread, []byte(`{"type":"transcript_mirror","filePath":"/tmp/agent-child-agent.jsonl","entries":[{"type":"assistant","uuid":"a-child","agentId":"child-agent","timestamp":"2026-08-24T12:00:03Z","message":{"id":"msg-child","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`))
	if err != nil {
		t.Fatalf("late child mirror: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("late child mirror was not projected: %+v", events)
	}
	if state.projections["/tmp/agent-child-agent.jsonl"] != nil || state.taskScopes["child-agent"].scope != "" {
		t.Fatalf("late terminal child retained projection state: %+v", state)
	}
	if state.projections["/tmp/agent-root.jsonl"] == nil {
		t.Fatal("closing late child removed its parent projection")
	}
}

func TestDirectNonForkingCommandKeepsItsCommandResult(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-usage", "/usage", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		return events
	}
	started := parse(`{"type":"command_lifecycle","command_uuid":"cmd-usage","state":"started"}`)
	if len(started) != 2 || started[0].Kind != provider.EventToolStart || started[0].ItemType != "Command" {
		t.Fatalf("non-fork command start = %+v", started)
	}
	if events := parse(`{"type":"assistant","message":{"id":"synthetic-usage","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Usage: 42%"}]}}`); len(events) != 0 {
		t.Fatalf("command output was not held until classification: %+v", events)
	}
	events := parse(`{"type":"result","subtype":"success","is_error":false,"result":"Usage: 42%"}`)
	if len(events) != 3 || events[0].Kind != provider.EventCommandResult || events[1].Kind != provider.EventToolComplete || events[1].Content != "Usage: 42%" || events[2].Kind != provider.EventTurnComplete {
		t.Fatalf("non-fork result events = %+v", events)
	}
	var resultMeta provider.CommandResultMeta
	if err := json.Unmarshal(events[0].Meta, &resultMeta); err != nil || !resultMeta.Suppressed {
		t.Fatalf("non-fork command result signal was not row-suppressed: meta=%s err=%v", events[0].Meta, err)
	}
	terminal := parse(`{"type":"command_lifecycle","command_uuid":"cmd-usage","state":"completed"}`)
	if len(terminal) != 1 || terminal[0].Kind != provider.EventCommandLifecycle {
		t.Fatalf("non-fork terminal events = %+v, want bookkeeping only", terminal)
	}
}

func TestDirectCommandWithoutSyntheticOutputSettlesBeforeTurnComplete(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-compact", "/compact", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		return events
	}
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-compact","state":"started"}`)
	events := parse(`{"type":"result","subtype":"success","is_error":false,"result":""}`)
	if len(events) != 2 || events[0].Kind != provider.EventToolComplete || events[1].Kind != provider.EventTurnComplete {
		t.Fatalf("compact result events = %+v, want command completion before turn completion", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode completion meta: %v", err)
	}
	if meta["is_error"] == true {
		t.Fatalf("successful output-free command rendered as failed: %s", events[0].Meta)
	}
	terminal := parse(`{"type":"command_lifecycle","command_uuid":"cmd-compact","state":"completed"}`)
	if len(terminal) != 1 || terminal[0].Kind != provider.EventCommandLifecycle {
		t.Fatalf("compact terminal events = %+v, want bookkeeping only", terminal)
	}
}

func TestFailedDirectCommandSettlesAsErrorBeforeTurnComplete(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-failed", "/compact", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session

	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		return events
	}
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-failed","state":"started"}`)
	events := parse(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["compaction failed"]}`)
	if len(events) != 2 || events[0].Kind != provider.EventToolComplete || events[1].Kind != provider.EventTurnComplete {
		t.Fatalf("failed result events = %+v, want command completion before turn completion", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode completion meta: %v", err)
	}
	if meta["is_error"] != true {
		t.Fatalf("failed command rendered as successful: %s", events[0].Meta)
	}
	turnComplete, ok := events[1].TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok || turnComplete.StopReason != "error" {
		t.Fatalf("failed command turn completion = %+v", events[1].TurnComplete)
	}
	terminal := parse(`{"type":"command_lifecycle","command_uuid":"cmd-failed","state":"cancelled"}`)
	if len(terminal) != 1 || terminal[0].Kind != provider.EventCommandLifecycle {
		t.Fatalf("failed command terminal events = %+v, want bookkeeping only", terminal)
	}
}

func TestTranscriptMirrorClassifiesAttributedBatchBeforePendingBounds(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-large", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"command_lifecycle","command_uuid":"cmd-large","state":"started"}`)); err != nil {
		t.Fatalf("start command: %v", err)
	}

	entries := make([]map[string]any, 0, maxPendingMirrorEntries+3)
	entries = append(entries, map[string]any{
		"type": "agent_metadata", "agentType": "general-purpose",
	})
	for i := 0; i <= maxPendingMirrorEntries; i++ {
		entries = append(entries, map[string]any{
			"type":        "user",
			"uuid":        fmt.Sprintf("u-%d", i),
			"agentId":     "root",
			"isSidechain": true,
			"timestamp":   "2026-08-24T12:00:00Z",
			"message": map[string]any{
				"role":    "user",
				"content": fmt.Sprintf("prompt %d", i),
			},
		})
	}
	entries = append(entries, map[string]any{
		"type":             "assistant",
		"uuid":             "a-attributed",
		"agentId":          "root",
		"isSidechain":      true,
		"attributionSkill": "code-review",
		"timestamp":        "2026-08-24T12:00:01Z",
		"message": map[string]any{
			"id": "msg-attributed", "role": "assistant", "model": "claude-opus-4-1",
			"content": []map[string]any{{"type": "text", "text": "reviewing"}},
		},
	})
	line, err := json.Marshal(map[string]any{
		"type": "transcript_mirror", "filePath": "/tmp/agent-root.jsonl", "entries": entries,
	})
	if err != nil {
		t.Fatalf("marshal mirror: %v", err)
	}
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse mirror: %v", err)
	}
	if got, want := len(events), len(entries); got != want {
		t.Fatalf("attributed batch emitted %d events, want launch plus %d renderable entries", got, len(entries)-1)
	}
}

func TestTranscriptMirrorPendingBufferHasByteAndCountBounds(t *testing.T) {
	state := (&Parser{}).ensureTranscriptMirrorState()
	entry := json.RawMessage(`{"uuid":"u","content":"` + string(make([]byte, maxPendingMirrorFileBytes)) + `"}`)
	state.bufferPending("large", "cmd", []json.RawMessage{entry})
	if len(state.pending["large"]) != 0 || state.totalPendingBytes != 0 {
		t.Fatalf("oversized pending entry retained: entries=%d bytes=%d", len(state.pending["large"]), state.totalPendingBytes)
	}
	small := json.RawMessage(`{"uuid":"u"}`)
	entries := make([]json.RawMessage, maxPendingMirrorEntries+1)
	for i := range entries {
		entries[i] = small
	}
	state.bufferPending("small", "cmd", entries)
	if got := len(state.pending["small"]); got != maxPendingMirrorEntries {
		t.Fatalf("pending entries = %d, want %d", got, maxPendingMirrorEntries)
	}
	state.clearPending("small")
	if state.totalPendingBytes != 0 {
		t.Fatalf("cleared pending bytes = %d, want 0", state.totalPendingBytes)
	}
}

func TestTranscriptMirrorPendingBoundSurfacesVisibleDegradationOnce(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/code-review high", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)); err != nil {
		t.Fatalf("start command: %v", err)
	}

	entries := make([]map[string]any, maxPendingMirrorEntries+1)
	for i := range entries {
		entries[i] = map[string]any{
			"type": "user", "uuid": fmt.Sprintf("u-%d", i), "agentId": "root",
			"message": map[string]any{"role": "user", "content": fmt.Sprintf("prompt %d", i)},
		}
	}
	line, err := json.Marshal(map[string]any{
		"type": "transcript_mirror", "filePath": "/tmp/agent-root.jsonl", "entries": entries,
	})
	if err != nil {
		t.Fatalf("marshal mirror: %v", err)
	}
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse mirror: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventNotification || events[0].ParentToolUseID != "claude-command:cmd-1" {
		t.Fatalf("degradation events = %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil || meta["kind"] != "transcript_mirror_degraded" {
		t.Fatalf("degradation meta = %s err=%v", events[0].Meta, err)
	}
	if !strings.Contains(events[0].Content, "could not be shown") {
		t.Fatalf("degradation content = %q", events[0].Content)
	}

	replay, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse repeated mirror: %v", err)
	}
	if len(replay) != 0 {
		t.Fatalf("repeated degradation emitted again: %+v", replay)
	}
}

func TestTranscriptMirrorContinuesManuallyBackgroundedAgent(t *testing.T) {
	parser := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}

	parse(`{"type":"assistant","message":{"id":"m-parent","role":"assistant","content":[{"type":"tool_use","id":"toolu-agent","name":"Agent","input":{"description":"review","subagent_type":"Explore","prompt":"inspect"}}]}}`)
	parse(`{"type":"system","subtype":"task_started","task_id":"agent-bg","task_type":"local_agent","tool_use_id":"toolu-agent"}`)
	parse(`{"type":"system","subtype":"task_updated","task_id":"agent-bg","patch":{"is_backgrounded":true}}`)

	events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-agent-bg.jsonl","entries":[{"type":"user","uuid":"u-bg","agentId":"agent-bg","timestamp":"2026-08-24T12:00:00Z","message":{"role":"user","content":"inspect"}},{"type":"assistant","uuid":"a-bg","agentId":"agent-bg","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-bg","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-bg-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`)
	if len(events) != 3 {
		t.Fatalf("background mirror emitted %d events: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolStart || events[0].ItemID != "toolu-agent" {
		t.Fatalf("background mirror marker = %+v", events[0])
	}
	for _, event := range events[1:] {
		if event.ParentToolUseID != "toolu-agent" {
			t.Fatalf("background mirror event escaped launch scope: %+v", event)
		}
	}

	// A cumulative/replayed mirror batch is deduped by transcript uuid.
	if replay := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-agent-bg.jsonl","entries":[{"type":"assistant","uuid":"a-bg","agentId":"agent-bg","timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-bg","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu-bg-read","name":"Read","input":{"file_path":"/repo/a.go"}}]}}]}`); len(replay) != 0 {
		t.Fatalf("replayed mirror row emitted twice: %+v", replay)
	}
}
