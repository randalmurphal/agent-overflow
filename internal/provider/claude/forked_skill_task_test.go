package claude

import (
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

// Live 2.1.284 captures (docs/references/fixtures/claude/README.md) of a
// forked skill that now runs as a local_agent task bound to its Skill call
// (claude-wire.md §E9).
const (
	fixtureForkedSkillTaskStarted   = "../../../docs/references/fixtures/claude/forked_skill_task_started_20261014.ndjson"
	fixtureForkedSkillDirectCommand = "../../../docs/references/fixtures/claude/forked_skill_direct_command_20261014.ndjson"
)

func replayParser(t *testing.T, parser *Parser, path string) []provider.ProviderEvent {
	t.Helper()
	var events []provider.ProviderEvent
	for i, line := range loadNDJSONFixture(t, path) {
		got, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d: parse error: %v", i+1, err)
		}
		events = append(events, got...)
	}
	return events
}

// resumePrompts are the §E6 resume prompt rows a replay emitted. A launch
// read as a resume emits one; no capture here resumes anything.
func resumePrompts(t *testing.T, events []provider.ProviderEvent) []provider.ProviderEvent {
	t.Helper()
	var prompts []provider.ProviderEvent
	for _, evt := range filterKinds(events, provider.EventUserText) {
		if len(evt.Meta) == 0 {
			continue
		}
		if decodeMetaMap(t, evt.Meta)["subagent_resume_prompt"] == true {
			prompts = append(prompts, evt)
		}
	}
	return prompts
}

// The fork's task_started binds its Skill call. That is a launch, never a
// §E6 resume: read as one, the Skill row was backgrounded and its forked
// result taken for a resume ack, which left it running forever.
func TestReplay_ForkedSkillTaskStartedIsALaunch(t *testing.T) {
	const (
		skillToolUse = "toolu_01YPMQv1H356djgueDWN7TbT"
		forkTaskID   = "a54d4ffe0c745a254"
		childAgent   = "toolu_01QMt7Qnawy9Y99kR1FokwLt"
	)
	events := replayParser(t, NewParser(), fixtureForkedSkillTaskStarted)

	if prompts := resumePrompts(t, events); len(prompts) != 0 {
		t.Fatalf("a fork launch emitted resume prompts: %+v", prompts)
	}
	var taskStamp, completion map[string]any
	for _, evt := range events {
		if evt.ItemID != skillToolUse {
			continue
		}
		meta := decodeMetaMap(t, evt.Meta)
		switch {
		case evt.Kind == provider.EventToolStart && meta["task_id"] != nil:
			taskStamp = meta
		case evt.Kind == provider.EventToolComplete:
			completion = meta
		}
	}
	if taskStamp == nil || taskStamp["task_id"] != forkTaskID || taskStamp["task_type"] != "local_agent" {
		t.Fatalf("Skill task stamp = %v, want task %s of type local_agent", taskStamp, forkTaskID)
	}
	if _, resumed := taskStamp["resumes_tool_use_id"]; resumed {
		t.Fatalf("Skill task stamp names a resumed launch: %v", taskStamp)
	}
	if completion == nil {
		t.Fatal("no completion for the Skill call")
	}
	if completion["is_background"] == true {
		t.Fatalf("forked Skill completion is background: %v", completion)
	}
	if _, ok := completion["skillFork"].(map[string]any); !ok {
		t.Fatalf("forked Skill completion has no skillFork stamp: %v", completion)
	}
	for _, evt := range filterKinds(events, provider.EventToolComplete) {
		if evt.ItemID == childAgent && decodeMetaMap(t, evt.Meta)["is_background"] == true {
			t.Fatalf("the fork's awaited Agent completed as background: %s", evt.Meta)
		}
	}
}

// A directly entered forked command: the fork's own task_started names no
// tool_use, so the command row learns its task from the fork's agentId,
// and the Agent the fork launches is announced (task_started) before the
// session mirror delivers its tool_use. That Agent is a launch too.
func TestReplay_ForkedSkillDirectCommandTaskAndChild(t *testing.T) {
	const (
		commandUUID = "11111111-2222-4333-8444-555555555555"
		launchID    = "claude-command:" + commandUUID
		forkTaskID  = "a2e2453ee0d214c2a"
	)
	session := &Session{}
	session.directCommands.note(commandUUID, "/probe-fork", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	events := replayParser(t, parser, fixtureForkedSkillDirectCommand)

	if prompts := resumePrompts(t, events); len(prompts) != 0 {
		t.Fatalf("the fork's child Agent was read as a resume: %+v", prompts)
	}
	var stamped bool
	for _, evt := range filterKinds(events, provider.EventToolStart) {
		if evt.ItemID == launchID && evt.ItemType == "Skill" {
			meta := decodeMetaMap(t, evt.Meta)
			stamped = meta["task_id"] == forkTaskID && meta["task_type"] == "local_agent"
		}
	}
	if !stamped {
		t.Fatalf("the command's Skill classification carries no task %s", forkTaskID)
	}
	var notified bool
	for _, evt := range filterKinds(events, provider.EventBackgroundTaskNotification) {
		if decodeMetaMap(t, evt.Meta)["task_id"] == forkTaskID {
			notified = evt.ItemID == launchID
		}
	}
	if !notified {
		t.Fatal("the fork's task_notification did not resolve to the command row")
	}
}

// A fork at depth 2 (a Skill call inside an agent) ends with a
// task_notification alone, and its result arrives on the sidechain with no
// tool_use_result. The notification must release the live-agent flag, or
// that result reads as an async ack and the Skill row never settles.
func TestTaskNotificationTerminalReleasesLiveAgentTask(t *testing.T) {
	for _, tc := range []struct {
		name       string
		notify     bool
		background bool
	}{
		{name: "terminal notification", notify: true, background: false},
		{name: "no terminal yet", notify: false, background: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()
			lines := []string{
				`{"type":"assistant","message":{"id":"msg-agent","role":"assistant","content":[{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{"description":"outer","prompt":"p"}}]},"parent_tool_use_id":null}`,
				`{"type":"system","subtype":"task_started","task_id":"task-agent","tool_use_id":"toolu_agent","description":"outer","task_type":"local_agent","spawn_depth":1}`,
				`{"type":"assistant","message":{"id":"msg-skill","role":"assistant","content":[{"type":"tool_use","id":"toolu_skill","name":"Skill","input":{"skill":"review"}}]},"parent_tool_use_id":"toolu_agent"}`,
				`{"type":"system","subtype":"task_started","task_id":"task-fork","tool_use_id":"toolu_skill","description":"/review","task_type":"local_agent","spawn_depth":2}`,
			}
			if tc.notify {
				lines = append(lines, `{"type":"system","subtype":"task_notification","task_id":"task-fork","tool_use_id":"toolu_skill","status":"completed","summary":"/review"}`)
			}
			lines = append(lines, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_skill","content":"Skill \"review\" completed (forked execution).\n\nResult:\nok"}]},"parent_tool_use_id":"toolu_agent"}`)
			var events []provider.ProviderEvent
			for i, line := range lines {
				got, err := parser.ParseLine(testThread, []byte(line))
				if err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				events = append(events, got...)
			}
			var completion map[string]any
			for _, evt := range filterKinds(events, provider.EventToolComplete) {
				if evt.ItemID == "toolu_skill" {
					completion = decodeMetaMap(t, evt.Meta)
				}
			}
			if completion == nil {
				t.Fatal("no completion for the depth-2 Skill call")
			}
			if got := completion["is_background"] == true; got != tc.background {
				t.Fatalf("depth-2 Skill completion is_background = %v, want %v", got, tc.background)
			}
		})
	}
}

// A call whose task_started arrives with no binding is a resume only when
// it was observed and starts no agent; the record keeps every recent call
// across a generation rotation.
func TestRecentToolUseNamesKeepsTheLastCapCalls(t *testing.T) {
	var names recentToolUseNames
	total := parserTaskMapCap*2 + 7
	for i := 0; i < total; i++ {
		names.remember(fmt.Sprintf("toolu_%d", i), "SendMessage")
	}
	for i := total - parserTaskMapCap; i < total; i++ {
		if got := names.name(fmt.Sprintf("toolu_%d", i)); got != "SendMessage" {
			t.Fatalf("call %d of the last %d forgotten (got %q)", i, parserTaskMapCap, got)
		}
	}
	if len(names.current)+len(names.previous) > 2*parserTaskMapCap {
		t.Fatalf("record holds %d calls, bound is %d", len(names.current)+len(names.previous), 2*parserTaskMapCap)
	}

	parser := NewParser()
	parser.rememberToolUseName("toolu_send", "SendMessage")
	parser.rememberToolUseName("toolu_agent", "Agent")
	parser.rememberToolUseName("toolu_skill", skillToolName)
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"toolu_send", true},
		{"toolu_agent", false},
		{"toolu_skill", false},
		{"toolu_unseen", false},
	} {
		if got := parser.isTaskResumeTool(tc.id); got != tc.want {
			t.Errorf("isTaskResumeTool(%s) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
