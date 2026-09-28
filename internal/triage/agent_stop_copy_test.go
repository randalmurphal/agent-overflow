package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// Live 2.1.280 captures (docs/references/fixtures/claude/README.md) of a
// background agent that parks on its own command while the main agent is
// mid-turn, so the CLI hands the park to the model again as XML on the
// isReplay echo.
const (
	// The copy arrives while the agent is still parked.
	fixtureParkedCopy = "../../docs/references/fixtures/claude/local_agent_parked_copy_20260925.ndjson"
	// The copy arrives after the command reported, before the wake.
	fixtureCopyBeforeWake = "../../docs/references/fixtures/claude/local_agent_copy_before_wake_20260925.ndjson"
)

// replayCapture feeds a capture through the real parser and router, one
// line every 100ms of event time, and returns every pushed write of an
// agent stop row, in order.
func replayCapture(t *testing.T, path string) (*store.Store, []store.Item) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parser := claude.NewParser()
	base := time.Now()
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		events, err := parser.ParseLine("t1", []byte(line))
		if err != nil {
			t.Fatalf("line %d: parse: %v", i+1, err)
		}
		for _, evt := range events {
			evt.Timestamp = base.Add(time.Duration(i) * 100 * time.Millisecond)
			if err := router.Handle(evt); err != nil {
				t.Fatalf("line %d: handle %s: %v", i+1, evt.Kind, err)
			}
			router.WaitForPendingSettles()
		}
	}
	var stops []store.Item
	for _, row := range itemUpserts(emissions.snapshot()) {
		if row.Kind == itemKindBackgroundDone && row.CompletionOf != "" && strings.HasPrefix(row.ToolName, "Agent") {
			stops = append(stops, row)
		}
	}
	return st, stops
}

// agentStopRows is the stored stop rows of launchID, parked and ending.
func agentStopRows(t *testing.T, st *store.Store, launchID string) (parked []store.Item, ending []store.Item) {
	t.Helper()
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if item.Kind != itemKindBackgroundDone || item.CompletionOf != launchID {
			continue
		}
		if item.Status == store.ItemStatusParked {
			parked = append(parked, item)
		} else {
			ending = append(ending, item)
		}
	}
	return parked, ending
}

// A copy of a parked stop that arrives while the agent is still parked
// writes nothing: the agent has one parked card, and its real final stop
// ends it with its final report.
func TestAgentStopCopyWhileParkedWritesNoSecondCard(t *testing.T) {
	const launch = "toolu_019b6ppXbQQYSo6twwNtbXSK"
	st, pushes := replayCapture(t, fixtureParkedCopy)
	parked, ending := agentStopRows(t, st, launch)
	if len(parked) != 1 {
		t.Fatalf("parked cards = %d, want 1 (the copy wrote another): %+v", len(parked), parked)
	}
	if len(ending) != 1 || stopReportShown(t, ending[0]) != "FINAL" {
		t.Fatalf("ending cards = %+v, want one showing FINAL", ending)
	}
	for _, row := range pushes {
		if row.ID != parked[0].ID && row.ID != ending[0].ID {
			t.Fatalf("pushed a stop row %s that is not the park or the end", row.ID)
		}
	}
}

// A copy of a parked stop that arrives after the agent's command reported
// but before the CLI woke the agent does not end it: the ending card is
// written only by the real final stop, after the wake, and shows the
// final report. The wake itself is recorded (the 2.1.280 shape names the
// launch).
func TestAgentStopCopyBeforeWakeDoesNotEndTheAgent(t *testing.T) {
	const launch = "toolu_01GV1XcazfvitjUCq6kqB6F4"
	st, pushes := replayCapture(t, fixtureCopyBeforeWake)
	parked, ending := agentStopRows(t, st, launch)
	if len(parked) != 1 {
		t.Fatalf("parked cards = %d, want 1: %+v", len(parked), parked)
	}
	if len(ending) != 1 || stopReportShown(t, ending[0]) != "FINAL" {
		t.Fatalf("ending cards = %+v, want one showing FINAL", ending)
	}
	for _, row := range pushes {
		if row.ID == ending[0].ID && stopReportShown(t, row) != "FINAL" {
			t.Fatalf("the ending card was pushed showing %q before the final stop", stopReportShown(t, row))
		}
	}
	wake, found, err := st.GetThreadItem("t1", provider.SubagentWakePromptItemID("toolu_01Be3N9Jz3LWALE7XCwMK9jE"))
	if err != nil || !found {
		t.Fatalf("the wake row of the agent's command: found=%v err=%v", found, err)
	}
	if wake.ParentID != launch {
		t.Fatalf("wake row parent = %q, want the launch %s", wake.ParentID, launch)
	}
	if wake.CreatedAt <= parked[0].CreatedAt || ending[0].CreatedAt <= wake.CreatedAt {
		t.Fatalf("park %d, wake %d, end %d are out of order", parked[0].CreatedAt, wake.CreatedAt, ending[0].CreatedAt)
	}
}

// An agent whose only background work is its own Monitor parks on it. A
// subagent's Monitor ack arrives with no tool_use_result, only its text
// (claude-wire.md §E7), so the capture's parked shell is rewritten into a
// Monitor launch with that ack. The watch keeps running as a background
// watch task, the agent's stop parks with one command, and the wake after
// the watch reports starts the run whose stop ends the agent.
func TestAgentParksOnItsOwnMonitor(t *testing.T) {
	const (
		launch  = "toolu_019b6ppXbQQYSo6twwNtbXSK"
		monitor = "toolu_01HpH31rs7JbWuYHbjWqcrQU"
	)
	data, err := os.ReadFile(fixtureParkedCopy)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	capture := string(data)
	for _, rewrite := range [][2]string{
		{`"name":"Bash","input":{"command":"sleep 60; echo SHELL-DONE","run_in_background":true}`,
			`"name":"Monitor","input":{"command":"sleep 60; echo SHELL-DONE","description":"shell done","timeout_ms":3600000}`},
		{`"content":"Command running in background with ID: bdq6lce49. Output is being written to: /tmp/claude-502/-tmp-spike-work/de4518bd-156b-4796-99cd-afe78125d5cb/tasks/bdq6lce49.output. You will be notified when it completes. To check interim output, use Read on that file path."`,
			`"content":"Monitor started (task bdq6lce49, timeout 3600000ms). You will be notified on each event. Keep working."`},
	} {
		if strings.Count(capture, rewrite[0]) != 1 {
			t.Fatalf("capture must hold %q once", rewrite[0])
		}
		capture = strings.Replace(capture, rewrite[0], rewrite[1], 1)
	}
	path := filepath.Join(t.TempDir(), "monitor_park.ndjson")
	if err := os.WriteFile(path, []byte(capture), 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}

	st, _ := replayCapture(t, path)
	watch := mustGetItem(t, st, "t1", monitor)
	if !watch.IsBackground || watch.ParentID != launch || decodeItemMetaMap(t, watch.Meta)["watch_task"] != true {
		t.Fatalf("Monitor row = background %v under %q, meta %s; want a background watch under the agent", watch.IsBackground, watch.ParentID, watch.Meta)
	}
	parked, ending := agentStopRows(t, st, launch)
	if len(parked) != 1 || decodeItemMetaMap(t, parked[0].Meta)[store.MetaKeyParkedCommands] != float64(1) {
		t.Fatalf("parked cards = %+v, want one waiting on the Monitor", parked)
	}
	if len(ending) != 1 || stopReportShown(t, ending[0]) != "FINAL" {
		t.Fatalf("ending cards = %+v, want one showing FINAL", ending)
	}
}

// The live capture of an agent whose nested async child outlives it
// (fixtures/claude/README.md): the parent's stop is final while the child
// runs, and the child's stop, delivered to the main session, lands at top
// level with its report, after the parent's card.
func TestNestedAgentThatOutlivesItsParentStopsAtTopLevel(t *testing.T) {
	const (
		parent = "toolu_01WqNAioa7bcQx89KAmks3gf"
		child  = "toolu_01Rq6eGzun6BM39KZSPRECCP"
	)
	st, _ := replayCapture(t, "../../docs/references/fixtures/claude/local_agent_nested_async_child_no_wake_20260908.ndjson")
	if launch := mustGetItem(t, st, "t1", child); launch.ParentID != parent {
		t.Fatalf("child launch under %q, want the parent %s", launch.ParentID, parent)
	}
	_, parentEnd := agentStopRows(t, st, parent)
	_, childEnd := agentStopRows(t, st, child)
	if len(parentEnd) != 1 || len(childEnd) != 1 {
		t.Fatalf("ending cards: parent %+v, child %+v; want one each", parentEnd, childEnd)
	}
	if childEnd[0].ParentID != "" || childEnd[0].CreatedAt <= parentEnd[0].CreatedAt {
		t.Fatalf("child card under %q at %d, parent card at %d; want the child at top level after the parent", childEnd[0].ParentID, childEnd[0].CreatedAt, parentEnd[0].CreatedAt)
	}
	if got := stopReportShown(t, childEnd[0]); got != "DEEP-DONE" {
		t.Fatalf("child card shows %q, want DEEP-DONE", got)
	}
}

// stopWithUsage is one agent stop whose notification reports usage, the
// shape both the structured envelope and its XML copy carry.
func stopWithUsage(t *testing.T, router *Router, launchID, taskID, summary, uuid string, durationMs int64, stash bool) {
	t.Helper()
	if stash {
		parkHandle(t, router, provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: launchID,
			Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": launchID, "status": "completed", "source": "task_updated"}),
		})
	}
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: launchID, Content: summary,
		Meta: parkMeta(t, map[string]any{
			"task_id": taskID, "tool_use_id": launchID, "status": "completed", "uuid": uuid,
			"usage": provider.SubagentProgressMeta{TotalTokens: 16571, ToolUses: 1, DurationMs: durationMs},
		}),
	})
}

// A copy that arrives while the parked card still waits behind the main
// agent's stream writes nothing: one parked card lands at the drain.
func TestAgentStopCopyOfAQueuedParkWritesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is talking"})
	stopWithUsage(t, router, "agent", "task-agent", "PAUSED", "u-park", 3309, true)
	stopWithUsage(t, router, "agent", "task-agent", "PAUSED", "u-copy", 3309, false)
	if queued := queuedRows(router, "t1"); len(queued) != 1 {
		t.Fatalf("queued rows = %v, want the one parked card", queued)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
	router.WaitForPendingSettles()
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 1 {
		t.Fatalf("parked cards = %d, want 1", len(stops))
	}
	if _, ended, err := st.GetThreadItem("t1", ToolCompletionID("agent")); err != nil || ended {
		t.Fatalf("the copy ended the agent: ended=%v err=%v", ended, err)
	}
}

// A copy of an agent's final stop writes nothing, whatever its summary;
// a notice that reports no usage (a kill's) is never taken for a copy.
func TestAgentStopCopyOfTheEndWritesNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	stopWithUsage(t, router, "agent", "task-agent", "The final report.", "u-end", 64370, true)
	stored := mustGetItem(t, st, "t1", ToolCompletionID("agent"))

	emissions.reset()
	stopWithUsage(t, router, "agent", "task-agent", `Agent "Spike agent" finished`, "u-copy", 64370, false)
	stopWithUsage(t, router, "agent", "task-agent", "Another report.", "u-copy2", 64370, false)
	if pushes := stopReportPushes(t, emissions.snapshot(), ToolCompletionID("agent")); len(pushes) != 0 {
		t.Fatalf("copies rewrote the stop: pushes %q", pushes)
	}
	if again := mustGetItem(t, st, "t1", ToolCompletionID("agent")); again.Meta != stored.Meta || again.UpdatedAt != stored.UpdatedAt {
		t.Fatalf("copies changed the stop:\n%s\n%s", stored.Meta, again.Meta)
	}
	if copied, err := router.agentStopCopy("t1", mustGetItem(t, st, "t1", "agent"), provider.SubagentProgressMeta{}); err != nil || copied {
		t.Fatalf("a notice without usage taken for a copy: copied=%v err=%v", copied, err)
	}
}

// The live captures of a user's stop of a parked agent's shell (fixture B)
// and of the parked agent itself (fixture E), claude-wire.md §E6b. A
// stopped shell settles killed and still wakes its agent, which reports
// once more and ends. A stopped parked agent ends killed with its shell,
// and nothing wakes.
func TestStopWhileParkedReplay(t *testing.T) {
	t.Run("the shell's stop wakes the agent", func(t *testing.T) {
		const (
			agent = "toolu_016CSmSHDDAAZg7LbVUJo5GZ"
			shell = "toolu_0123VW7nWBUoufVYzPoJr5Bu"
		)
		st, _ := replayCapture(t, "../../docs/references/fixtures/claude/local_agent_owned_shell_stop_wake_20260908.ndjson")
		parked, ending := agentStopRows(t, st, agent)
		if len(parked) != 1 || len(ending) != 1 {
			t.Fatalf("agent stops: parked %d, ending %d; want 1 and 1", len(parked), len(ending))
		}
		if got := stopReportShown(t, parked[0]); got != "WAITING" {
			t.Fatalf("parked card shows %q, want WAITING", got)
		}
		if ending[0].Status != statusCompleted || stopReportShown(t, ending[0]) != "WOKE stopped" {
			t.Fatalf("ending %s shows %q, want completed with WOKE stopped", ending[0].Status, stopReportShown(t, ending[0]))
		}
		if done := mustGetItem(t, st, "t1", ToolCompletionID(shell)); done.Status != statusKilled {
			t.Fatalf("shell settled %s, want killed", done.Status)
		}
		wake := mustGetItem(t, st, "t1", "user:subagent-wake:"+shell)
		if wake.ParentID != agent || wake.CreatedAt <= parked[0].CreatedAt || wake.CreatedAt >= ending[0].CreatedAt {
			t.Fatalf("wake under %q at %d; want under the agent between the park (%d) and the end (%d)", wake.ParentID, wake.CreatedAt, parked[0].CreatedAt, ending[0].CreatedAt)
		}
		expectNoLiveRun(t, st, agent)
	})
	t.Run("the parked agent's stop kills it and its shell", func(t *testing.T) {
		const (
			agent = "toolu_015fBb9Ui8ehHifGXVD1gT5q"
			shell = "toolu_01TKknxUfLmm8UQcLmSAzAz4"
		)
		st, _ := replayCapture(t, "../../docs/references/fixtures/claude/local_agent_parked_stop_task_20260908.ndjson")
		parked, ending := agentStopRows(t, st, agent)
		if len(parked) != 1 || len(ending) != 1 || ending[0].Status != statusKilled {
			t.Fatalf("agent stops: parked %+v, ending %+v; want one parked and one killed", parked, ending)
		}
		if done := mustGetItem(t, st, "t1", ToolCompletionID(shell)); done.Status != statusKilled {
			t.Fatalf("shell settled %s, want killed", done.Status)
		}
		if _, found, err := st.GetThreadItem("t1", "user:subagent-wake:"+shell); err != nil || found {
			t.Fatalf("a wake was written: found=%v err=%v", found, err)
		}
		expectNoLiveRun(t, st, agent)
	})
}

func expectNoLiveRun(t *testing.T, st *store.Store, agent string) {
	t.Helper()
	if live, err := st.LiveAgentRunRows("t1", agent); err != nil || len(live) != 0 {
		t.Fatalf("live run rows %v (err %v), want none", live, err)
	}
}
