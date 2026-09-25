package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
)

// The frames an interrupt owes the background tasks a turn announced
// (claude_tasks.go; claude-wire.md §Background task ownership, 2.1.280
// captures A, B and D of 2026-09-24).

const interruptLine = `{"type":"control_request","request_id":"int-1","request":{"subtype":"interrupt"}}`

// A streamed text block, so validateClaudeFrames sees a coherent turn.
var claudeTextBlockLines = []string{
	`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-lead","role":"assistant"}}}`,
	`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Launching."}}}`,
	`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
	`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
	`{"type":"assistant","message":{"id":"msg-lead","role":"assistant","content":[{"type":"text","text":"Launching."}]}}`,
}

// An async agent launch as the CLI announces it: the level set first,
// then task_started, then the async ack on the tool_result.
var claudeAgentLaunchLines = []string{
	`{"type":"assistant","message":{"id":"msg-agent","role":"assistant","content":[{"type":"tool_use","id":"tu-agent","name":"Agent","input":{"description":"sweep","prompt":"Sweep."}}]}}`,
	`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-agent","task_type":"local_agent","description":"sweep"}]}`,
	`{"type":"system","subtype":"task_started","task_id":"task-agent","tool_use_id":"tu-agent","description":"sweep","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent"}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-agent","content":[{"type":"text","text":"Async agent launched successfully.\nagentId: task-agent"}]}]},"tool_use_result":{"isAsync":true,"status":"async_launched","agentId":"task-agent","description":"sweep"}}`,
}

// The agent's own background shell: its tool_use hangs under the launch.
var claudeOwnedShellLines = []string{
	`{"type":"assistant","parent_tool_use_id":"tu-agent","message":{"id":"msg-owned","role":"assistant","content":[{"type":"tool_use","id":"tu-owned","name":"Bash","input":{"command":"sleep 75","run_in_background":true}}]}}`,
	`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-agent","task_type":"local_agent","description":"sweep"},{"task_id":"task-owned","task_type":"local_bash","description":"sleep 75"}]}`,
	`{"type":"system","subtype":"task_started","task_id":"task-owned","owned_by_subagent":true,"tool_use_id":"tu-owned","description":"sleep 75","is_backgrounded":true,"task_type":"local_bash"}`,
	`{"type":"user","parent_tool_use_id":"tu-agent","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-owned","content":"Command running in background with ID: task-owned."}]}}`,
}

func claudeHoldScenario(name string, lines ...[]string) *scenario.Scenario {
	var emit []string
	for _, group := range lines {
		emit = append(emit, group...)
	}
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     name,
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: emit}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
		}}},
		AfterTurns: "silent",
	}
}

type wireFrame struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	TaskID    string `json:"task_id"`
	ToolUseID string `json:"tool_use_id"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	// OutputFile is a pointer so "" and absent stay apart: a stopped
	// foreground command names an empty file, a killed shell a real one.
	OutputFile *string `json:"output_file"`
	Patch      struct {
		Status  string `json:"status"`
		EndTime int64  `json:"end_time"`
	} `json:"patch"`
	Tasks []struct {
		TaskID string `json:"task_id"`
	} `json:"tasks"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			StillQueued *[]string `json:"still_queued"`
		} `json:"response"`
	} `json:"response"`
	SubagentStats *struct {
		Killed struct {
			System int `json:"system"`
		} `json:"killed"`
	} `json:"subagent_stats"`
}

func decodeFrame(t *testing.T, line string) wireFrame {
	t.Helper()
	var f wireFrame
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return f
}

func (f wireFrame) taskIDs() []string {
	ids := make([]string, 0, len(f.Tasks))
	for _, task := range f.Tasks {
		ids = append(ids, task.TaskID)
	}
	return ids
}

func expectLevelSet(t *testing.T, line string, want ...string) {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Subtype != "background_tasks_changed" || strings.Join(f.taskIDs(), ",") != strings.Join(want, ",") {
		t.Fatalf("want the level set %v, got %s", want, line)
	}
}

func expectKilled(t *testing.T, line, taskID string) {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Subtype != "task_updated" || f.TaskID != taskID || f.Patch.Status != "killed" || f.Patch.EndTime == 0 {
		t.Fatalf("want task_updated{killed} for %s, got %s", taskID, line)
	}
}

func expectStopped(t *testing.T, line, taskID, toolUseID, summary string) string {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Subtype != "task_notification" || f.TaskID != taskID || f.ToolUseID != toolUseID || f.Status != "stopped" || f.Summary != summary || f.OutputFile == nil {
		t.Fatalf("want task_notification{stopped} for %s/%s naming an output_file, got %s", taskID, toolUseID, line)
	}
	return *f.OutputFile
}

func expectInterruptAck(t *testing.T, line string) {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Type != "control_response" || f.Response.Subtype != "success" || f.Response.RequestID != "int-1" ||
		f.Response.Response.StillQueued == nil || len(*f.Response.Response.StillQueued) != 0 {
		t.Fatalf("want the interrupt ack with still_queued:[], got %s", line)
	}
}

func expectInterruptedResult(t *testing.T, line string, killedAgents int) {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Type != "result" || f.Subtype != "error_during_execution" {
		t.Fatalf("want the interrupted result, got %s", line)
	}
	if killedAgents == 0 {
		if f.SubagentStats != nil {
			t.Fatalf("no agent was killed, yet the result carries subagent_stats: %s", line)
		}
		return
	}
	if f.SubagentStats == nil || f.SubagentStats.Killed.System != killedAgents {
		t.Fatalf("want subagent_stats.killed.system=%d, got %s", killedAgents, line)
	}
}

// Capture A plus B's foreground command: the agent and the shell it owns
// die before the ack, the foreground command stops after it, the main
// thread's own background shell survives, and the result counts the
// kill.
func TestClaudeInterruptKillsAsyncAgentsAndTheirShells(t *testing.T) {
	mainShell := []string{
		`{"type":"assistant","message":{"id":"msg-main","role":"assistant","content":[{"type":"tool_use","id":"tu-main","name":"Bash","input":{"command":"sleep 40","run_in_background":true}}]}}`,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-agent","task_type":"local_agent","description":"sweep"},{"task_id":"task-main","task_type":"local_bash","description":"sleep 40"}]}`,
		`{"type":"system","subtype":"task_started","task_id":"task-main","tool_use_id":"tu-main","description":"sleep 40","is_backgrounded":true,"task_type":"local_bash"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-main","content":"Command running in background with ID: task-main."}]}}`,
	}
	ownedShell := []string{
		claudeOwnedShellLines[0],
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-agent","task_type":"local_agent","description":"sweep"},{"task_id":"task-main","task_type":"local_bash","description":"sleep 40"},{"task_id":"task-owned","task_type":"local_bash","description":"sleep 75"}]}`,
		claudeOwnedShellLines[2],
		claudeOwnedShellLines[3],
	}
	foreground := []string{
		`{"type":"assistant","message":{"id":"msg-fg","role":"assistant","content":[{"type":"tool_use","id":"tu-fg","name":"Bash","input":{"command":"sleep 30"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"task-fg","tool_use_id":"tu-fg","description":"sleep 30","is_backgrounded":false,"task_type":"local_bash"}`,
	}
	env := writeScenarioFile(t, claudeHoldScenario("claude-interrupt-kills", claudeTextBlockLines, claudeAgentLaunchLines, mainShell, ownedShell, foreground), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"task_id":"task-fg"`, testTimeout)
	p.send(interruptLine)

	expectLevelSet(t, p.expectLine(testTimeout), "task-main", "task-owned")
	expectKilled(t, p.expectLine(testTimeout), "task-agent")
	agentFile := expectStopped(t, p.expectLine(testTimeout), "task-agent", "tu-agent", "sweep")
	if !strings.HasSuffix(agentFile, "task-agent.output") {
		t.Fatalf("the agent's notification names %q, want its task output file", agentFile)
	}
	expectLevelSet(t, p.expectLine(testTimeout), "task-main")
	expectKilled(t, p.expectLine(testTimeout), "task-owned")
	shellFile := expectStopped(t, p.expectLine(testTimeout), "task-owned", "tu-owned", "sleep 75")
	if _, err := os.Stat(shellFile); err != nil {
		t.Fatalf("the killed shell's output file is not there for the app to read: %v", err)
	}
	expectInterruptAck(t, p.expectLine(testTimeout))
	if file := expectStopped(t, p.expectLine(testTimeout), "task-fg", "tu-fg", "sleep 30"); file != "" {
		t.Fatalf("a stopped foreground command names output_file %q, want \"\"", file)
	}
	expectInterruptedResult(t, p.expectLine(testTimeout), 1)

	// Nothing left to kill: the main thread's shell is still running and
	// the next interrupt is acknowledged at once.
	p.send(interruptLine)
	expectInterruptAck(t, p.expectLine(testTimeout))

	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

// Capture D: a PARKED agent (it reported while its shell still ran) dies
// with a second task_updated{killed} and no notification, its shell dies
// with it, and no wake follows.
func TestClaudeInterruptKillsAParkedAgent(t *testing.T) {
	park := []string{
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-owned","task_type":"local_bash","description":"sleep 75"}]}`,
		`{"type":"system","subtype":"task_updated","task_id":"task-agent","patch":{"status":"completed","end_time":1790279985248}}`,
		`{"type":"system","subtype":"task_notification","task_id":"task-agent","tool_use_id":"tu-agent","status":"completed","output_file":"${CWD}/task-agent.output","summary":"PARKED","usage":{"total_tokens":10207,"tool_uses":1,"duration_ms":2526}}`,
	}
	env := writeScenarioFile(t, claudeHoldScenario("claude-interrupt-parked", claudeTextBlockLines, claudeAgentLaunchLines, claudeOwnedShellLines, park), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"summary":"PARKED"`, testTimeout)
	p.send(interruptLine)

	expectKilled(t, p.expectLine(testTimeout), "task-agent")
	expectLevelSet(t, p.expectLine(testTimeout))
	expectKilled(t, p.expectLine(testTimeout), "task-owned")
	expectStopped(t, p.expectLine(testTimeout), "task-owned", "tu-owned", "sleep 75")
	expectInterruptAck(t, p.expectLine(testTimeout))
	expectInterruptedResult(t, p.expectLine(testTimeout), 1)
	p.closeStdinAndExpectExit(0, testTimeout)

	for _, line := range p.all {
		f := decodeFrame(t, line)
		if f.Subtype == "task_notification" && f.TaskID == "task-agent" && f.Status == "stopped" {
			t.Fatalf("a parked agent's kill carries no notification, got %s", line)
		}
		if f.Subtype == "task_started" && f.TaskID == "task-agent" && f.ToolUseID == "" {
			t.Fatalf("a killed parked agent never wakes, got %s", line)
		}
	}
	validateClaudeFrames(t, p.all)
}

// A parked agent the CLI woke (a task_started naming no tool_use) is
// running again: the interrupt kills it like any running agent, with
// its notification.
func TestClaudeInterruptKillsAWokenAgent(t *testing.T) {
	parkThenWake := []string{
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-owned","task_type":"local_bash","description":"sleep 75"}]}`,
		`{"type":"system","subtype":"task_updated","task_id":"task-agent","patch":{"status":"completed","end_time":1790279985248}}`,
		`{"type":"system","subtype":"task_notification","task_id":"task-agent","tool_use_id":"tu-agent","status":"completed","output_file":"${CWD}/task-agent.output","summary":"PARKED"}`,
		`{"type":"system","subtype":"task_updated","task_id":"task-owned","patch":{"status":"completed","end_time":1790279990000}}`,
		`{"type":"system","subtype":"task_notification","task_id":"task-owned","tool_use_id":"tu-owned","status":"completed","summary":"done"}`,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`,
		`{"type":"system","subtype":"task_started","task_id":"task-agent","description":"sweep","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent","prompt":"<task-notification>done</task-notification>"}`,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-agent","task_type":"local_agent","description":"sweep"}]}`,
	}
	env := writeScenarioFile(t, claudeHoldScenario("claude-interrupt-woken", claudeTextBlockLines, claudeAgentLaunchLines, claudeOwnedShellLines, parkThenWake), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"prompt":"<task-notification>`, testTimeout)
	p.expectLineContaining(`"tasks":[{"task_id":"task-agent"`, testTimeout)
	p.send(interruptLine)

	expectLevelSet(t, p.expectLine(testTimeout))
	expectKilled(t, p.expectLine(testTimeout), "task-agent")
	expectStopped(t, p.expectLine(testTimeout), "task-agent", "tu-agent", "sweep")
	expectInterruptAck(t, p.expectLine(testTimeout))
	expectInterruptedResult(t, p.expectLine(testTimeout), 1)
	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func stopTaskLine(requestID, taskID string) string {
	return `{"type":"control_request","request_id":"` + requestID + `","request":{"subtype":"stop_task","task_id":"` + taskID + `"}}`
}

// endHeldTurn interrupts the held turn so the frames end in a result the
// app's parser can check.
func endHeldTurn(t *testing.T, p *mockProc) {
	t.Helper()
	p.send(interruptLine)
	p.expectLineContaining(`"type":"result"`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func expectStopTaskAck(t *testing.T, line, requestID string) {
	t.Helper()
	f := decodeFrame(t, line)
	if f.Type != "control_response" || f.Response.Subtype != "success" || f.Response.RequestID != requestID {
		t.Fatalf("want the stop_task ack for %s, got %s", requestID, line)
	}
}

// parkedAgentLines park the agent on its owned shell (capture E's start).
var parkedAgentLines = []string{
	`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-owned","task_type":"local_bash","description":"sleep 75"}]}`,
	`{"type":"system","subtype":"task_updated","task_id":"task-agent","patch":{"status":"completed","end_time":1790279985248}}`,
	`{"type":"system","subtype":"task_notification","task_id":"task-agent","tool_use_id":"tu-agent","status":"completed","output_file":"${CWD}/task-agent.output","summary":"PARKED"}`,
}

// Capture E: stop_task on a parked agent kills it with task_updated{killed}
// and no notification, empties the level set, and kills its shell with
// it, all before the ack. Nothing is left to stop.
func TestClaudeStopTaskKillsAParkedAgentAndItsShell(t *testing.T) {
	env := writeScenarioFile(t, claudeHoldScenario("claude-stop-parked", claudeTextBlockLines, claudeAgentLaunchLines, claudeOwnedShellLines, parkedAgentLines), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"summary":"PARKED"`, testTimeout)
	p.send(stopTaskLine("stop-1", "task-agent"))
	expectKilled(t, p.expectLine(testTimeout), "task-agent")
	expectLevelSet(t, p.expectLine(testTimeout))
	expectKilled(t, p.expectLine(testTimeout), "task-owned")
	expectStopped(t, p.expectLine(testTimeout), "task-owned", "tu-owned", "sleep 75")
	expectStopTaskAck(t, p.expectLine(testTimeout), "stop-1")

	p.send(stopTaskLine("stop-2", "task-owned"))
	expectStopTaskAck(t, p.expectLine(testTimeout), "stop-2")
	endHeldTurn(t, p)
}

// A running agent's stop_task is written as the interrupt writes its kill:
// the agent killed with its notification, then each shell it owns.
func TestClaudeStopTaskKillsARunningAgentAndItsShell(t *testing.T) {
	env := writeScenarioFile(t, claudeHoldScenario("claude-stop-running", claudeTextBlockLines, claudeAgentLaunchLines, claudeOwnedShellLines), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"task_id":"task-owned"`, testTimeout)
	p.expectLineContaining(`"tool_use_id":"tu-owned","content"`, testTimeout)
	p.send(stopTaskLine("stop-1", "task-agent"))
	expectLevelSet(t, p.expectLine(testTimeout), "task-owned")
	expectKilled(t, p.expectLine(testTimeout), "task-agent")
	expectStopped(t, p.expectLine(testTimeout), "task-agent", "tu-agent", "sweep")
	expectLevelSet(t, p.expectLine(testTimeout))
	expectKilled(t, p.expectLine(testTimeout), "task-owned")
	expectStopped(t, p.expectLine(testTimeout), "task-owned", "tu-owned", "sleep 75")
	expectStopTaskAck(t, p.expectLine(testTimeout), "stop-1")
	endHeldTurn(t, p)
}

// Capture B: stop_task on the shell kills the shell alone; the agent that
// owns it stays in the level set.
func TestClaudeStopTaskKillsOneShell(t *testing.T) {
	env := writeScenarioFile(t, claudeHoldScenario("claude-stop-shell", claudeTextBlockLines, claudeAgentLaunchLines, claudeOwnedShellLines), "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"tool_use_id":"tu-owned","content"`, testTimeout)
	p.send(stopTaskLine("stop-1", "task-owned"))
	expectLevelSet(t, p.expectLine(testTimeout), "task-agent")
	expectKilled(t, p.expectLine(testTimeout), "task-owned")
	expectStopped(t, p.expectLine(testTimeout), "task-owned", "tu-owned", "sleep 75")
	expectStopTaskAck(t, p.expectLine(testTimeout), "stop-1")

	p.send(stopTaskLine("stop-2", "task-unknown"))
	expectStopTaskAck(t, p.expectLine(testTimeout), "stop-2")
	endHeldTurn(t, p)
}
