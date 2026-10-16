package claude

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// local_agent_resumed_by_typed_fork_20261014 (claude-wire.md §E6, §E9):
// turn 1 launches agent a431; turn 2 runs the typed fork /resume-fork,
// which resumes a431 with a SendMessage the mirror delivers after its
// task_started; the resumed a431 then resumes the finished fork a533 from
// stdout, and the resumed fork's rows reach only the mirror.
const (
	fixtureResumedByTypedFork = "../../../docs/references/fixtures/claude/local_agent_resumed_by_typed_fork_20261014.ndjson"

	resumeForkCommandUUID = "11111111-2222-4333-8444-555555555551"
	resumeForkCommandRow  = "claude-command:" + resumeForkCommandUUID
	resumeForkAgentLaunch = "toolu_014LbM2xPb8AjJCetu4PsyGo"
	resumeForkAgentTask   = "a43133614d8d4014b"
	resumeForkForkTask    = "a533b097681aff049"
	resumeForkCarrier     = "toolu_01CydpVX9mVrMBPLbJ8Bk5Ax"
	resumeForkForkCarrier = "toolu_019zWFJCuFeNM9DvSvK4K9Tx"
	// The typed fork's last message, written after its task_notification,
	// and the resumed fork's, written after its task_updated terminal.
	resumeForkForkTail    = "msg_011Cg3BsDG6bWndaTApeZybM#0"
	resumeForkResumedTail = "msg_011Cg3BsdwAb6pHqQTcoaPsX#0"
)

// rootOnlyLaunchLookup adapts a lookup that knows only the launch id.
func rootOnlyLaunchLookup(lookup func(string) (string, bool, error)) func(string) (AgentLaunchRecord, bool, error) {
	if lookup == nil {
		return nil
	}
	return func(taskID string) (AgentLaunchRecord, bool, error) {
		root, found, err := lookup(taskID)
		return AgentLaunchRecord{ToolUseID: root}, found, err
	}
}

func resumeForkParser(lookup func(string) (string, bool, error)) *Parser {
	session := &Session{}
	session.directCommands.note(resumeForkCommandUUID, "/resume-fork a43133614d8d4014b", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	parser.agentLaunchLookup = rootOnlyLaunchLookup(lookup)
	return parser
}

// replayFrom parses the fixture from line index first on, as a parser that
// started after the earlier lines were written would.
func replayFrom(t *testing.T, parser *Parser, path string, first int) []provider.ProviderEvent {
	t.Helper()
	var events []provider.ProviderEvent
	for i, line := range loadNDJSONFixture(t, path)[first:] {
		got, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d: %v", first+i+1, err)
		}
		events = append(events, got...)
	}
	return events
}

// resumeFields merges every meta-only tool_start for itemID.
func resumeFields(t *testing.T, events []provider.ProviderEvent, itemID string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	for _, evt := range filterKinds(events, provider.EventToolStart) {
		if evt.ItemID == itemID && evt.ItemType == "" {
			for k, v := range decodeMetaMap(t, evt.Meta) {
				fields[k] = v
			}
		}
	}
	return fields
}

func completionOf(t *testing.T, events []provider.ProviderEvent, itemID string) map[string]any {
	t.Helper()
	for _, evt := range filterKinds(events, provider.EventToolComplete) {
		if evt.ItemID == itemID {
			return decodeMetaMap(t, evt.Meta)
		}
	}
	t.Fatalf("no completion for %s", itemID)
	return nil
}

func hasRow(events []provider.ProviderEvent, itemID, parent string) bool {
	for _, evt := range events {
		if evt.ItemID == itemID && evt.ParentToolUseID == parent {
			return true
		}
	}
	return false
}

func noErrorEvents(t *testing.T, events []provider.ProviderEvent) {
	t.Helper()
	if errs := filterKinds(events, provider.EventError); len(errs) != 0 {
		t.Fatalf("replay emitted errors: %+v", errs)
	}
}

// A resume the typed fork sends arrives as a projected call. Its ack is
// the resumed round's background carrier, as a resume on stdout is, so
// the round stays running until the agent's terminal.
func TestReplay_TypedForkResumeCarrierIsBackgrounded(t *testing.T) {
	events := replayParser(t, resumeForkParser(nil), fixtureResumedByTypedFork)
	noErrorEvents(t, events)

	if completion := completionOf(t, events, resumeForkCarrier); completion["is_background"] != true {
		t.Fatalf("the fork's resume ack settled in place: %+v", completion)
	}
	resume := resumeFields(t, events, resumeForkCarrier)
	if resume["resumes_tool_use_id"] != resumeForkAgentLaunch || resume["transcript_root_id"] != resumeForkAgentLaunch {
		t.Fatalf("carrier resume meta = %+v, want both pointing at %s", resume, resumeForkAgentLaunch)
	}
	// Triage keeps only a row's task correlation ahead of the row, so the
	// resume identity follows the call's own row.
	callAt, identityAt := -1, -1
	for i, evt := range events {
		if evt.Kind != provider.EventToolStart || evt.ItemID != resumeForkCarrier {
			continue
		}
		if evt.ItemType != "" && callAt < 0 {
			callAt = i
		}
		if evt.ItemType == "" && decodeMetaMap(t, evt.Meta)["resumes_tool_use_id"] != nil {
			identityAt = i
		}
	}
	if callAt < 0 || identityAt < callAt {
		t.Fatalf("the resume identity (event %d) arrived before the call's row (event %d)", identityAt, callAt)
	}
	if prompts := resumePrompts(t, events); len(prompts) != 2 {
		t.Fatalf("resume prompts = %d, want one per resume: %+v", len(prompts), prompts)
	}
}

// The typed fork's last message lands after its task_notification and must
// still project under the command row.
func TestReplay_TypedForkLastMessageFollowsItsTerminal(t *testing.T) {
	events := replayParser(t, resumeForkParser(nil), fixtureResumedByTypedFork)
	if !hasRow(events, resumeForkForkTail, resumeForkCommandRow) {
		t.Fatal("the fork's last message, written after its terminal, was dropped")
	}
}

// The resumed fork has no launching tool_use, so its rows reach only the
// mirror and return to the command row it first ran under.
func TestReplay_ResumedTypedForkProjectsUnderItsCommandRow(t *testing.T) {
	events := replayParser(t, resumeForkParser(nil), fixtureResumedByTypedFork)
	resume := resumeFields(t, events, resumeForkForkCarrier)
	if resume["transcript_root_id"] != resumeForkCommandRow {
		t.Fatalf("the fork's resume names root %v, want %s", resume["transcript_root_id"], resumeForkCommandRow)
	}
	if completion := completionOf(t, events, resumeForkForkCarrier); completion["is_background"] != true {
		t.Fatalf("the fork's resume carrier settled in place: %+v", completion)
	}
	if !hasRow(events, resumeForkResumedTail, resumeForkCommandRow) {
		t.Fatal("the resumed fork's rows were dropped")
	}
}

// A parser that started after the agent's launch learns the resume when the
// mirror delivers the fork's call. The thread's history names the root; a
// history that cannot still leaves a backgrounded carrier, and a lookup
// failure is not a parse error.
func TestReplay_TypedForkResumeOfAnEarlierProcessAgent(t *testing.T) {
	const turnTwo = 29
	for _, tc := range []struct {
		name     string
		lookup   func(string) (string, bool, error)
		wantRoot any
	}{
		{"history names the launch", func(task string) (string, bool, error) {
			if task == resumeForkAgentTask {
				return resumeForkAgentLaunch, true, nil
			}
			return "", false, nil
		}, resumeForkAgentLaunch},
		{"history has no launch", func(string) (string, bool, error) { return "", false, nil }, nil},
		{"history read fails", func(string) (string, bool, error) { return "", false, errors.New("disk gone") }, nil},
		{"no history", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := replayFrom(t, resumeForkParser(tc.lookup), fixtureResumedByTypedFork, turnTwo)
			noErrorEvents(t, events)
			if completion := completionOf(t, events, resumeForkCarrier); completion["is_background"] != true {
				t.Fatalf("the resume read as a launch: its ack settled in place: %+v", completion)
			}
			resume := resumeFields(t, events, resumeForkCarrier)
			if resume["description"] != "Test agent call" || resume["subagent_type"] != "general-purpose" {
				t.Fatalf("carrier lost the resumed agent's identity: %+v", resume)
			}
			if resume["transcript_root_id"] != tc.wantRoot || resume["resumes_tool_use_id"] != tc.wantRoot {
				t.Fatalf("carrier root = %v / %v, want %v", resume["transcript_root_id"], resume["resumes_tool_use_id"], tc.wantRoot)
			}
			if prompts := resumePrompts(t, events); len(prompts) == 0 || prompts[0].ParentToolUseID != resumeForkCarrier {
				t.Fatalf("the resume's prompt row is missing: %+v", prompts)
			}
		})
	}
}

// After a restart, a resumed typed fork is recognised by its root: the
// thread's history names the command row, so its rows project there.
func TestReplay_ResumedTypedForkOfAnEarlierProcess(t *testing.T) {
	const turnThree = 52
	found := func(task string) (string, bool, error) {
		if task == resumeForkForkTask {
			return resumeForkCommandRow, true, nil
		}
		return "", false, nil
	}
	events := replayFrom(t, resumeForkParser(found), fixtureResumedByTypedFork, turnThree)
	noErrorEvents(t, events)
	if !hasRow(events, resumeForkResumedTail, resumeForkCommandRow) {
		t.Fatal("the resumed fork's rows were dropped")
	}
	if completion := completionOf(t, events, resumeForkForkCarrier); completion["is_background"] != true {
		t.Fatalf("the fork's resume carrier settled in place: %+v", completion)
	}

	unknown := replayFrom(t, resumeForkParser(nil), fixtureResumedByTypedFork, turnThree)
	for _, evt := range unknown {
		if evt.ParentToolUseID == resumeForkCommandRow {
			t.Fatalf("with no root known, a row was projected under the command row: %+v", evt)
		}
	}
}

// The typed fork's Agent announces its agent before the mirror delivers the
// Agent call. Its rows wait for the call, then project under it.
func TestReplay_TypedForkChildAgentProjects(t *testing.T) {
	const (
		commandUUID = "11111111-2222-4333-8444-555555555555"
		commandRow  = "claude-command:" + commandUUID
		child       = "toolu_01QDcQJK7gt5NaJEzQfFmEaQ"
		childShell  = "toolu_018pqQ4XzTekpkWBYQ2h8WUV"
	)
	session := &Session{}
	session.directCommands.note(commandUUID, "/probe-fork", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	events := replayParser(t, parser, fixtureForkedSkillDirectCommand)
	noErrorEvents(t, events)

	if !hasRow(events, childShell, child) {
		t.Fatal("the child agent's tool call did not project under its launch")
	}
	var childText, forkTail bool
	for _, evt := range filterKinds(events, provider.EventTextDelta) {
		childText = childText || (evt.ParentToolUseID == child && strings.Contains(evt.Content, "grandchild-ok"))
		forkTail = forkTail || (evt.ParentToolUseID == commandRow && strings.Contains(evt.Content, "fork-ok, grandchild-ok"))
	}
	if !childText {
		t.Fatal("the child agent's last message, written after its terminal, was dropped")
	}
	if !forkTail {
		t.Fatal("the fork's last message, written after its terminal, was dropped")
	}
	if prompts := resumePrompts(t, events); len(prompts) != 0 {
		t.Fatalf("the child launch read as a resume: %+v", prompts)
	}
}

// A task binding outlives parserTaskMapCap later tasks of any kind: every
// Bash call is one. Past two generations a resume finds its launch in the
// thread's history.
func TestTaskBindingSurvivesLaterTasks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fillers  int
		agents   bool
		lookup   func(string) (string, bool, error)
		wantKept bool
	}{
		{"within the bound", parserTaskMapCap + parserTaskMapCap/2, false, nil, true},
		{"past the bound", 3 * parserTaskMapCap, true, func(string) (string, bool, error) { return "toolu_launch", true, nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()
			parser.agentLaunchLookup = rootOnlyLaunchLookup(tc.lookup)
			parse := func(line string) []provider.ProviderEvent {
				t.Helper()
				events, err := parser.ParseLine(testThread, []byte(line))
				if err != nil {
					t.Fatalf("ParseLine: %v", err)
				}
				return events
			}
			parse(`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"d","prompt":"p","run_in_background":true}}]}}`)
			parse(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"toolu_launch","task_type":"local_agent"}`)
			for i := range tc.fillers {
				if tc.agents {
					parse(fmt.Sprintf(`{"type":"assistant","message":{"id":"mf%d","role":"assistant","content":[{"type":"tool_use","id":"toolu_f_%d","name":"Agent","input":{"description":"f","prompt":"f"}}]}}`, i, i))
					parse(fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":"filler-%d","tool_use_id":"toolu_f_%d","task_type":"local_agent"}`, i, i))
				} else {
					parse(fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":"bash-%d","tool_use_id":"toolu_bash_%d","task_type":"local_bash"}`, i, i))
				}
			}
			kept := parser.taskToolUseRef("agent-1").ToolUseID == "toolu_launch" && parser.taskTranscriptRoot("agent-1") == "toolu_launch"
			if kept != tc.wantKept {
				t.Fatalf("after %d later tasks the binding kept = %v, want %v", tc.fillers, kept, tc.wantKept)
			}
			parse(`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"toolu_send","name":"SendMessage","input":{"to":"agent-1","message":"again"}}]}}`)
			events := parse(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"toolu_send","task_type":"local_agent","description":"d"}`)
			resume := resumeFields(t, events, "toolu_send")
			if resume["resumes_tool_use_id"] != "toolu_launch" || resume["transcript_root_id"] != "toolu_launch" {
				t.Fatalf("resume after %d tasks = %+v, want root toolu_launch", tc.fillers, resume)
			}
		})
	}
}

// A child's first rows can reach the mirror before the fork's Agent call
// does. They wait for the call instead of being dropped.
func TestReplay_TypedForkChildRowsBeforeItsLaunch(t *testing.T) {
	const (
		commandUUID  = "11111111-2222-4333-8444-555555555555"
		child        = "toolu_01QDcQJK7gt5NaJEzQfFmEaQ"
		childPrompt  = "3f0d5391-11cd-4667-b1cd-7f5435ca4215"
		launchBatch  = 12
		childPrompts = 13
	)
	session := &Session{}
	session.directCommands.note(commandUUID, "/probe-fork", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	lines := loadNDJSONFixture(t, fixtureForkedSkillDirectCommand)
	if !strings.Contains(string(lines[launchBatch]), child) || !strings.Contains(string(lines[childPrompts]), childPrompt) {
		t.Fatal("fixture lines moved: the launch batch and the child's prompt batch are not where this test swaps them")
	}
	lines[launchBatch], lines[childPrompts] = lines[childPrompts], lines[launchBatch]
	var events []provider.ProviderEvent
	for i, line := range lines {
		got, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		events = append(events, got...)
	}
	if !hasRow(events, childPrompt, child) {
		t.Fatal("the child's prompt, mirrored before its launch, was dropped")
	}
}

// A parked agent's wake whose binding is gone carries its
// `<task-notification>` prompt; it is not taken for the running typed
// fork's own task, which would leave the fork's row with the wrong task.
func TestUnboundWakeIsNotTheTypedForkTask(t *testing.T) {
	const commandUUID = "cmd-wake"
	session := &Session{}
	session.directCommands.note(commandUUID, "/probe-fork", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	for _, line := range []string{
		`{"type":"command_lifecycle","command_uuid":"` + commandUUID + `","state":"started"}`,
		`{"type":"system","subtype":"task_started","task_id":"parked-agent","task_type":"local_agent","prompt":"<task-notification>\n<task-id>shell-1</task-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}`,
		`{"type":"system","subtype":"task_started","task_id":"fork-task","task_type":"local_agent","prompt":"Base directory for this skill"}`,
	} {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
	}
	if got := parser.transcriptMirror.commands[commandUUID].forkTaskID; got != "fork-task" {
		t.Fatalf("the command's fork task = %q, want fork-task", got)
	}
}

// A forked Skill call inside a typed fork is bound by its own task_started
// before its metadata arrives; it is not the command's fork.
func TestBoundForkInsideATypedForkIsNotTheCommandFork(t *testing.T) {
	const commandUUID = "cmd-nested"
	session := &Session{}
	session.directCommands.note(commandUUID, "/outer", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	for _, line := range []string{
		`{"type":"command_lifecycle","command_uuid":"` + commandUUID + `","state":"started"}`,
		`{"type":"system","subtype":"task_started","task_id":"outer","task_type":"local_agent"}`,
		`{"type":"transcript_mirror","filePath":"/tmp/agent-outer.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`,
		`{"type":"transcript_mirror","filePath":"/tmp/agent-outer.jsonl","entries":[{"type":"user","uuid":"u-outer","agentId":"outer","isSidechain":true,"attributionSkill":"outer","timestamp":"2026-10-14T12:00:00Z","message":{"role":"user","content":"go"}}]}`,
		`{"type":"system","subtype":"task_started","task_id":"inner","tool_use_id":"toolu_inner_skill","task_type":"local_agent"}`,
		`{"type":"transcript_mirror","filePath":"/tmp/agent-inner.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"}]}`,
		`{"type":"transcript_mirror","filePath":"/tmp/agent-inner.jsonl","entries":[{"type":"user","uuid":"u-inner","agentId":"inner","isSidechain":true,"attributionSkill":"inner","timestamp":"2026-10-14T12:00:01Z","message":{"role":"user","content":"inner"}}]}`,
	} {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
	}
	if got := parser.transcriptMirror.commands[commandUUID].agentID; got != "outer" {
		t.Fatalf("the command's fork = %q, want outer", got)
	}
}

// A terminal keeps a projection open for the agent's last message, bounded
// by maxTerminalMirrorTasks: the oldest is closed past it.
func TestTerminalProjectionsAreBounded(t *testing.T) {
	parser := NewParser()
	state := parser.ensureTranscriptMirrorState()
	agent := func(i int) string { return fmt.Sprintf("agent-%d", i) }
	for i := range maxTerminalMirrorTasks + 1 {
		if _, err := state.newProjection("/tmp/"+agent(i)+".jsonl", "launch-"+agent(i), agent(i), ""); err != nil {
			t.Fatalf("new projection: %v", err)
		}
		parser.finishMirroredTask(testThread, agent(i), false)
	}
	if state.projections["/tmp/agent-0.jsonl"] != nil || state.taskScopes[agent(0)].scope != "" {
		t.Fatal("the oldest terminal projection was not closed past the bound")
	}
	for i := 1; i <= maxTerminalMirrorTasks; i++ {
		if state.projections["/tmp/"+agent(i)+".jsonl"] == nil {
			t.Fatalf("terminal projection %d closed within the bound", i)
		}
	}
	if len(state.terminalTasks) != maxTerminalMirrorTasks {
		t.Fatalf("terminal queue holds %d, want %d", len(state.terminalTasks), maxTerminalMirrorTasks)
	}
}

// A resumed agent with a launching tool_use streams on stdout, so the
// projection its backgrounded run left open is closed, not fed twice.
func TestResumedStdoutAgentClosesItsOpenProjection(t *testing.T) {
	a := startParkedMirrorAgent(t)
	a.parse(`{"type":"system","subtype":"task_updated","task_id":"` + parkedMirrorTask + `","patch":{"status":"killed","end_time":1790000002000}}`)
	if !a.retired() {
		t.Fatal("precondition: the killed agent's projection stays open for its last message")
	}
	a.parse(`{"type":"assistant","message":{"id":"m-send","role":"assistant","content":[{"type":"tool_use","id":"toolu_send","name":"SendMessage","input":{"to":"` + parkedMirrorTask + `","message":"again"}}]}}`)
	a.parse(`{"type":"system","subtype":"task_started","task_id":"` + parkedMirrorTask + `","tool_use_id":"toolu_send","task_type":"local_agent"}`)
	if a.projected() || a.bound() {
		t.Fatal("the resumed agent's earlier projection is still open")
	}
	if rows := a.mirrorText("a-again", "AGAIN"); len(rows) != 0 {
		t.Fatalf("a resumed stdout agent's mirror rows were projected: %+v", rows)
	}
}

// A parked agent's projection is left as it is on a resume: no capture
// shows where a parked agent's resumed rounds go, and closing it would drop
// them if they reach only the mirror.
func TestResumedParkedAgentKeepsItsProjection(t *testing.T) {
	a := startParkedMirrorAgent(t)
	if !a.projected() || a.retired() {
		t.Fatal("precondition: the parked agent's projection is open and not retired")
	}
	a.parse(`{"type":"assistant","message":{"id":"m-send","role":"assistant","content":[{"type":"tool_use","id":"toolu_send","name":"SendMessage","input":{"to":"` + parkedMirrorTask + `","message":"again"}}]}}`)
	a.parse(`{"type":"system","subtype":"task_started","task_id":"` + parkedMirrorTask + `","tool_use_id":"toolu_send","task_type":"local_agent"}`)
	if !a.projected() || !a.bound() {
		t.Fatal("a resume closed the parked agent's projection")
	}
}
