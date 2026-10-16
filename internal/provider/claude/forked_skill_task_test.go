package claude

import (
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// Live 2.1.284 captures (docs/references/fixtures/claude/README.md) of a
// forked skill that now runs as a local_agent task bound to its Skill call
// (claude-wire.md §E9).
const (
	fixtureForkedSkillTaskStarted   = "../../../docs/references/fixtures/claude/forked_skill_task_started_20261014.ndjson"
	fixtureForkedSkillDirectCommand = "../../../docs/references/fixtures/claude/forked_skill_direct_command_20261014.ndjson"
	fixtureTypedForkStopTask        = "../../../docs/references/fixtures/claude/forked_skill_direct_command_stop_task_20261014.ndjson"
	fixtureTypedForkInterrupt       = "../../../docs/references/fixtures/claude/forked_skill_direct_command_interrupt_20261014.ndjson"
	// Live 2.1.300 capture: after the typed fork's task_notification the
	// CLI replays the fork's turns, and its Agent's, on stdout.
	fixtureTypedForkStdoutReplay = "../../../docs/references/fixtures/claude/forked_skill_direct_command_stdout_replay_20261016.ndjson"
	// Live 2.1.300 capture: a fork the Skill tool launched streams its
	// rows on stdout, now with agent_id.
	fixtureSkillToolForkAgentID = "../../../docs/references/fixtures/claude/forked_skill_tool_agent_id_20261016.ndjson"
)

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

// An older CLI announces no task for a fork (claude-wire.md §E9, 2.1.237).
// Its typed fork is still the fork card, but its row names no task: a
// Stop would send stop_task for a task the CLI does not have.
func TestReplay_TypedForkWithoutATaskNamesNoTask(t *testing.T) {
	const (
		commandUUID = "11111111-2222-4333-8444-555555555555"
		launchID    = "claude-command:" + commandUUID
	)
	session := &Session{}
	session.directCommands.note(commandUUID, "/probe-fork", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	var classified map[string]any
	for i, line := range loadNDJSONFixture(t, fixtureForkedSkillDirectCommand) {
		if strings.Contains(string(line), `"task_id":"a2e2453ee0d214c2a"`) && strings.Contains(string(line), `"task_started"`) {
			continue
		}
		events, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		for _, evt := range filterKinds(events, provider.EventToolStart) {
			if evt.ItemID == launchID && evt.ItemType == "Skill" {
				classified = decodeMetaMap(t, evt.Meta)
			}
		}
	}
	if classified == nil || classified["skillFork"] == nil {
		t.Fatalf("the typed fork was not classified as a fork: %+v", classified)
	}
	if _, has := classified["task_id"]; has {
		t.Fatalf("a fork with no announced task names one: %+v", classified)
	}
}

// A stopped typed fork's turn still ends with a successful result, and no
// launch exists for the CLI to answer with an error. The fork's own
// stopped notification is what settles its command row as failed; a fork
// that finished settles it as completed.
func TestReplay_TypedForkSettlesByItsTaskTerminal(t *testing.T) {
	const commandUUID = "11111111-2222-4333-8444-555555555555"
	for _, tc := range []struct {
		name, fixture, command string
		failed                 bool
	}{
		{"completed", fixtureForkedSkillDirectCommand, "/probe-fork", false},
		{"stop_task", fixtureTypedForkStopTask, "/slow-fork", true},
		{"interrupt", fixtureTypedForkInterrupt, "/slow-fork", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &Session{}
			session.directCommands.note(commandUUID, tc.command, provider.SendOptions{})
			parser := NewParser()
			parser.peerTurns = session
			events := replayParser(t, parser, tc.fixture)
			var completions []map[string]any
			for _, evt := range filterKinds(events, provider.EventToolComplete) {
				if evt.ItemID == "claude-command:"+commandUUID {
					completions = append(completions, decodeMetaMap(t, evt.Meta))
				}
			}
			if len(completions) != 1 || completions[0]["directCommandFork"] != true {
				t.Fatalf("want one completion of the forked command row, got %+v", completions)
			}
			if failed := completions[0]["is_error"] == true; failed != tc.failed {
				t.Fatalf("command row completion is_error = %v, want %v: %+v", failed, tc.failed, completions[0])
			}
		})
	}
}

// Claude 2.1.300 replays a typed fork's turns on stdout after the fork's
// task_notification, each row naming its agent in agent_id; the fork's own
// rows name "forked-command-<skill>" as their parent, which no item has.
// The session mirror already projected every one of them under the
// command row, so the replay emits nothing: no event names that parent,
// the fork's answer is one text item, and the child's tool call starts
// once.
func TestReplay_TypedForkStdoutReplayEmitsNothing(t *testing.T) {
	const (
		commandUUID = "88888888-2222-4333-8444-555555555555"
		launchID    = "claude-command:" + commandUUID
		childBash   = "toolu_01X375JJbyhGDV4HGxVWPJvq"
	)
	session := &Session{}
	session.directCommands.note(commandUUID, "/forkagent", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	events := replayParser(t, parser, fixtureTypedForkStdoutReplay)

	answers := map[string]struct{}{}
	bashStarts := 0
	for _, evt := range events {
		if strings.HasPrefix(evt.ItemID, "forked-command-") || strings.HasPrefix(evt.ParentToolUseID, "forked-command-") {
			t.Errorf("%s %s parent %q: the stdout replay reached the event stream", evt.Kind, evt.ItemID, evt.ParentToolUseID)
		}
		if evt.Kind == provider.EventTextDelta && evt.Content == "done" {
			answers[evt.ParentToolUseID+" "+evt.ItemID] = struct{}{}
		}
		if evt.Kind == provider.EventToolStart && evt.ItemID == childBash && evt.ItemType != "" {
			bashStarts++
		}
	}
	if len(answers) != 1 {
		t.Fatalf("fork answer text items = %v, want one under %s", answers, launchID)
	}
	for key := range answers {
		if !strings.HasPrefix(key, launchID+" ") {
			t.Fatalf("fork answer %q is not under the command row %s", key, launchID)
		}
	}
	if bashStarts != 1 {
		t.Fatalf("the child's Bash call started %d times, want once", bashStarts)
	}
}

// A fork the Skill tool launched streams its rows on stdout, and on
// 2.1.300 they carry agent_id too. The mirror does not project that fork,
// so stdout is its only surface and every row is kept.
func TestReplay_SkillToolForkStdoutRowsWithAgentIDAreKept(t *testing.T) {
	const (
		skillCall = "toolu_01FebFRbLUJ4sZfbqEpGciUZ"
		forkBash  = "toolu_01VcvDcdJ6LC4cqLPZ4ykn7o"
	)
	events := replayParser(t, NewParser(), fixtureSkillToolForkAgentID)
	var bashStarted, bashCompleted, answered bool
	for _, evt := range events {
		switch {
		case evt.Kind == provider.EventToolStart && evt.ItemID == forkBash:
			bashStarted = evt.ParentToolUseID == skillCall
		case evt.Kind == provider.EventToolComplete && evt.ItemID == forkBash:
			bashCompleted = true
		case evt.Kind == provider.EventContentBlockStop && evt.ParentToolUseID == skillCall && evt.Content == "done":
			answered = true
		}
	}
	if !bashStarted || !bashCompleted || !answered {
		t.Fatalf("fork rows on stdout: Bash started under the Skill call %v, completed %v, answer %v; want all", bashStarted, bashCompleted, answered)
	}
}

// A call whose task_started arrives with no binding is a resume only when
// it was observed and starts no agent; the record keeps every recent call
// across a generation rotation.
func TestRecentToolUseNamesKeepsTheLastCapCalls(t *testing.T) {
	var names recentMap[string]
	total := parserTaskMapCap*2 + 7
	for i := 0; i < total; i++ {
		names.set(fmt.Sprintf("toolu_%d", i), "SendMessage")
	}
	for i := total - parserTaskMapCap; i < total; i++ {
		if got, _ := names.get(fmt.Sprintf("toolu_%d", i)); got != "SendMessage" {
			t.Fatalf("call %d of the last %d forgotten (got %q)", i, parserTaskMapCap, got)
		}
	}
	if names.len() > 2*parserTaskMapCap {
		t.Fatalf("record holds %d calls, bound is %d", names.len(), 2*parserTaskMapCap)
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
