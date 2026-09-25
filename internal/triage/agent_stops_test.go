package triage

// What a parked sibling records about its run (agent_stops.go): the
// commands it waits on, its report and its report's head, when the run
// began and whether a wake began it. Sequences reuse the park helpers.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// parkNotify is the stop's notification alone, with an output_file as
// the CLI sends for an agent.
func parkNotify(t *testing.T, router *Router, threadID, boundID, taskID, summary, uuid string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: threadID, ItemID: boundID, Content: summary,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "uuid": uuid, "output_file": "/tmp/agent-" + taskID + ".output"}),
	})
}

func parkTerminal(t *testing.T, router *Router, threadID, boundID, taskID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID, ItemID: boundID,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "source": "task_updated"}),
	})
}

func stopPreview(t *testing.T, st *store.Store, stop store.Item) string {
	t.Helper()
	if stop.PayloadID == "" {
		return ""
	}
	payload, err := st.GetPayloadMeta(stop.ThreadID, stop.PayloadID)
	if err != nil {
		t.Fatalf("payload of %s: %v", stop.ID, err)
	}
	var decoded struct {
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(payload.Meta), &decoded); err != nil {
		t.Fatalf("decode the payload of %s: %v", stop.ID, err)
	}
	return decoded.Preview
}

// Two parked runs and the final one. Each parked sibling names its own
// run's report, the head of the report the stop carried, the commands it
// waits on and when the run began; the second, woken run says so. The
// first sibling reads the same after the runs that follow it, and the
// final stop's ending sibling is the one an unparked agent writes.
func TestParkedStopRecordsItsRun(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	launch := mustGetItem(t, st, "t1", "agent")

	seedAgentReport(t, st, "t1", "agent", "report-old", "First look: nothing yet.")
	seedAgentReport(t, st, "t1", "agent", "report-1", "Waiting for the gate to finish.")
	parkTerminal(t, router, "t1", "agent", "task-agent")
	emissions.reset()
	parkNotify(t, router, "t1", "agent", "task-agent", "Waiting for the gate to finish.", "u1")

	first := mustGetItem(t, st, "t1", parkedStopID("agent", "u1", 0))
	if first.Status != store.ItemStatusParked || first.CompletionOf != "agent" || first.ParentID != "" {
		t.Fatalf("first stop = %q of %q under %q, want parked of the launch at top level", first.Status, first.CompletionOf, first.ParentID)
	}
	meta := decodeItemMetaMap(t, first.Meta)
	if meta["task_id"] != "task-agent" || meta[store.MetaKeyParkedCommands] != float64(2) || meta[store.MetaKeyParkedReportItemID] != "report-1" ||
		meta[store.MetaKeyRunStartedAt] != float64(launch.CreatedAt) || meta["notification_output_loaded"] != true {
		t.Errorf("first stop meta = %v, want task-agent, 2 commands, report-1, started at %d, output loaded", meta, launch.CreatedAt)
	}
	if _, woke := meta[store.MetaKeyRunWoke]; woke {
		t.Errorf("the first run was not woken: %v", meta)
	}
	if got := stopPreview(t, st, first); got != "Waiting for the gate to finish." {
		t.Errorf("first stop preview = %q", got)
	}
	if got := countEvents(emissions.snapshot(), eventchan.ProviderBackgroundTasksChanged.String()); got != 0 {
		t.Errorf("the parked stop sent %d background_tasks_changed nudges, want none: it changes no task's liveness", got)
	}

	nextMillisecond()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	wake := mustGetItem(t, st, "t1", provider.SubagentWakePromptItemID("shell"))
	seedAgentReport(t, st, "t1", "agent", "report-2", "Round two: waiting on the second gate.")
	parkTerminal(t, router, "t1", "agent", "task-agent")
	parkNotify(t, router, "t1", "agent", "task-agent", "Round two: waiting on the second gate.", "u2")

	second := mustGetItem(t, st, "t1", parkedStopID("agent", "u2", 0))
	meta = decodeItemMetaMap(t, second.Meta)
	if meta[store.MetaKeyParkedCommands] != float64(1) || meta[store.MetaKeyParkedReportItemID] != "report-2" ||
		meta[store.MetaKeyRunStartedAt] != float64(wake.CreatedAt) || meta[store.MetaKeyRunWoke] != true {
		t.Errorf("second stop meta = %v, want 1 command, report-2, woken at %d", meta, wake.CreatedAt)
	}
	if second.CreatedAt <= wake.CreatedAt {
		t.Errorf("second stop at %d, want after the wake at %d", second.CreatedAt, wake.CreatedAt)
	}

	parkShellDone(t, router, "t1", "shell-2", "task-shell-2", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell-2", "task-shell-2", nil)
	parkTerminal(t, router, "t1", "agent", "task-agent")
	parkNotify(t, router, "t1", "agent", "task-agent", "Round three report: the gate passed.", "u3")

	final := parkCompletions(t, st, "t1")["agent"]
	if final.ID != ToolCompletionID("agent") || final.Status != statusCompleted {
		t.Fatalf("final stop = %q %q, want the completed ending sibling", final.ID, final.Status)
	}
	if got := stopPreview(t, st, final); got != "Round three report: the gate passed." {
		t.Errorf("final stop preview = %q", got)
	}
	if again := mustGetItem(t, st, "t1", first.ID); again.Meta != first.Meta || again.Summary != first.Summary || again.PayloadID != first.PayloadID || again.Status != first.Status {
		t.Errorf("the first parked sibling changed after the runs that followed it:\n%+v\n%+v", first, again)
	}
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 2 {
		t.Errorf("parked siblings = %d, want 2", len(stops))
	}
	assertNoAgentBells(t, st, "t1", "task-agent")
}

// A stop and the wake after it, and that wake and the next stop, can share
// a millisecond. Each row is written after the one it follows, so the tray
// (Store.CurrentParkedStop) and the per-stop cards, which order them by
// creation time, file the wake in the run it starts.
func TestStopsAndWakesInOneMillisecondKeepTheirOrder(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	pinParkClock(t, time.Now().Add(time.Hour))

	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	first := mustGetItem(t, st, "t1", parkedStopID("agent", "u1", 0))
	wake := mustGetItem(t, st, "t1", provider.SubagentWakePromptItemID("shell"))
	if wake.CreatedAt <= first.CreatedAt {
		t.Errorf("wake at %d, want after the stop at %d", wake.CreatedAt, first.CreatedAt)
	}
	if _, parked, err := st.CurrentParkedStop("t1", "agent", "agent"); err != nil || parked {
		t.Errorf("after the wake the agent reads parked=%v err=%v, want running", parked, err)
	}

	parkStop(t, router, "t1", "agent", "task-agent", "WAITING AGAIN", "u2")
	second := mustGetItem(t, st, "t1", parkedStopID("agent", "u2", 0))
	if second.CreatedAt <= wake.CreatedAt {
		t.Errorf("second stop at %d, want after the wake at %d", second.CreatedAt, wake.CreatedAt)
	}
	if stop, parked, err := st.CurrentParkedStop("t1", "agent", "agent"); err != nil || !parked || stop.ID != second.ID {
		t.Errorf("after the second stop the agent reads %q parked=%v err=%v, want parked at %q", stop.ID, parked, err, second.ID)
	}
}

// A resumed agent's wake follows the stop it wakes from, the carrier's,
// even in that stop's millisecond: the wake row sits under the transcript
// root, which is not the row the stop completes.
func TestCarrierWakeFollowsTheCarriersStop(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	parkStop(t, router, "t1", "root", "task-agent", "ROUND 1", "u1")
	nextMillisecond()
	resumeAgent(t, router, "t1", "carrier", map[string]any{
		"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root",
		"description": "Spike root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root",
	})
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	pinParkClock(t, time.Now().Add(time.Hour))

	parkStop(t, router, "t1", "carrier", "task-agent", "ROUND 2", "u2")
	parkShellDone(t, router, "t1", "shell", "task-shell", "root")
	parkWake(t, router, "t1", "carrier", "task-agent", "shell", "task-shell", nil)
	stop := mustGetItem(t, st, "t1", parkedStopID("carrier", "u2", 0))
	wake := mustGetItem(t, st, "t1", provider.SubagentWakePromptItemID("shell"))
	if wake.ParentID != "root" {
		t.Fatalf("wake filed under %q, want the transcript root", wake.ParentID)
	}
	if wake.CreatedAt <= stop.CreatedAt {
		t.Errorf("wake at %d, want after the carrier's stop at %d", wake.CreatedAt, stop.CreatedAt)
	}
	if _, parked, err := st.CurrentParkedStop("t1", "carrier", "root"); err != nil || parked {
		t.Errorf("after the wake the carrier reads parked=%v err=%v, want running", parked, err)
	}
}

// A stop the parser could not bind to its tool_use (a fresh parser lost
// the task map) resolves its row by task id, past the rows of the agent's
// earlier stop and wake that carry the same task id.
func TestUnboundStopResolvesTheLaunchByTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	nextMillisecond()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	nextMillisecond()

	parkStop(t, router, "t1", "", "task-agent", "Done after the wake.", "u2")
	final, ended := parkCompletions(t, st, "t1")["agent"]
	if !ended || final.ID != ToolCompletionID("agent") || final.Status != statusCompleted {
		t.Fatalf("unbound final stop wrote %+v (ended=%v), want the launch's completed ending sibling", final, ended)
	}
}

// A run that wrote no text has no report, rather than an earlier run's.
func TestParkedStopOfARunWithoutTextNamesNoReport(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	seedAgentReport(t, st, "t1", "agent", "report-1", "Round one.")
	parkStop(t, router, "t1", "agent", "task-agent", "Round one.", "u1")

	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	nextMillisecond()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	parkStop(t, router, "t1", "agent", "task-agent", "", "u2")

	meta := decodeItemMetaMap(t, mustGetItem(t, st, "t1", parkedStopID("agent", "u2", 0)).Meta)
	if _, has := meta[store.MetaKeyParkedReportItemID]; has {
		t.Errorf("a run without text named a report: %v", meta)
	}
}

// A re-delivered stop writes nothing: the sibling is keyed by the
// notification's uuid, and the row it wrote first stays as it was.
func TestParkedStopIsWrittenOnce(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	first := mustGetItem(t, st, "t1", parkedStopID("agent", "u1", 0))
	nextMillisecond()
	parkShellDone(t, router, "t1", "shell-2", "task-shell-2", "agent")
	parkNotify(t, router, "t1", "agent", "task-agent", "WAITING, delivered again", "u1")

	stops := parkedStops(t, st, "t1", "agent")
	if len(stops) != 1 {
		t.Fatalf("a re-delivered stop wrote %d parked siblings, want 1", len(stops))
	}
	if again := stops[0]; again.CreatedAt != first.CreatedAt || again.Meta != first.Meta || stopPreview(t, st, again) != "WAITING" {
		t.Errorf("a re-delivered stop rewrote the parked sibling:\n%+v\n%+v", first, again)
	}
}

// A nested agent's parked sibling files under its parent agent, like its
// launch, so the parent's card holds it.
func TestNestedParkedStopFilesUnderTheParentAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "parent", "task-parent", "")
	parkLaunchAgent(t, router, "t1", "child", "task-child", "parent")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "child")
	parkStop(t, router, "t1", "child", "task-child", "WAITING", "u1")

	stops := parkedStops(t, st, "t1", "child")
	if len(stops) != 1 || stops[0].ParentID != "parent" {
		t.Fatalf("nested parked siblings = %+v, want one under the parent agent", stops)
	}
}

// A resume carrier's run parks at a sibling of its own, names the
// carrier, and reads its report at the transcript root from the carrier's
// start: the root's earlier report is not this run's.
func TestCarrierParkedStopIsTheCarriersOwn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	seedAgentReport(t, st, "t1", "root", "report-1", "ROUND 1")
	parkStop(t, router, "t1", "root", "task-agent", "ROUND 1", "u1")

	nextMillisecond()
	resumeAgent(t, router, "t1", "carrier", map[string]any{
		"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root",
		"description": "Spike root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root",
	})
	carrier := mustGetItem(t, st, "t1", "carrier")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	parkStop(t, router, "t1", "carrier", "task-agent", "ROUND 2", "u2")

	stops := parkedStops(t, st, "t1", "carrier")
	if len(stops) != 1 {
		t.Fatalf("carrier parked siblings = %d, want 1", len(stops))
	}
	if want := carrier.Summary + " -> parked"; stops[0].Summary != want {
		t.Errorf("carrier parked sibling = %q, want %q", stops[0].Summary, want)
	}
	meta := decodeItemMetaMap(t, stops[0].Meta)
	if meta[store.MetaKeyRunStartedAt] != float64(carrier.CreatedAt) {
		t.Errorf("carrier run started at %v, want the carrier's %d", meta[store.MetaKeyRunStartedAt], carrier.CreatedAt)
	}
	if _, has := meta[store.MetaKeyParkedReportItemID]; has {
		t.Errorf("the carrier's run named the root's earlier report: %v", meta)
	}
	if dones := parkCompletions(t, st, "t1"); dones["root"].Status != statusCompleted || len(parkedStops(t, st, "t1", "root")) != 0 {
		t.Errorf("the root's own run ended once with no parked sibling: %v", dones)
	}
}

// trayProbe records each tray frame a router emits naming launchID, with
// whether row rowID was written when it was emitted.
type trayProbe struct {
	mu     sync.Mutex
	frames []trayProbeFrame
	nudges int
}

type trayProbeFrame struct {
	frame   BackgroundTrayEvent
	withRow bool
}

func (p *trayProbe) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames, p.nudges = nil, 0
}

// served is the run state the last recorded frame serves launchID at, and
// whether its row was written when it went out; "" when no frame did.
func (p *trayProbe) served(launchID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.frames) - 1; i >= 0; i-- {
		if row, ok := trayRow(p.frames[i].frame, launchID); ok {
			return store.AgentRunState(row), p.frames[i].withRow
		}
	}
	return "", false
}

func newTrayProbeRouter(t *testing.T, rowID string) (*Router, *store.Store, *trayProbe) {
	t.Helper()
	st := storetest.Clone(t)
	createTestThread(t, st, "t1")
	probe := &trayProbe{}
	router := NewRouter(st, func(channel eventchan.Channel, data any) {
		switch channel {
		case eventchan.ProviderBackgroundTasksChanged:
			probe.mu.Lock()
			probe.nudges++
			probe.mu.Unlock()
		case eventchan.ProviderBackgroundTray:
			_, found, err := st.GetThreadItem("t1", rowID)
			probe.mu.Lock()
			probe.frames = append(probe.frames, trayProbeFrame{frame: data.(BackgroundTrayEvent), withRow: err == nil && found})
			probe.mu.Unlock()
		}
	})
	t.Cleanup(router.flushAllUsage)
	t.Cleanup(router.DrainWireItemRefresh)
	return router, st, probe
}

// A parked stop queued behind an open stream announces its agent when it
// lands: the drain's push of the row names the launch, and the frame
// serves it parked.
func TestQueuedParkedStopAnnouncesTheAgentWhenItLands(t *testing.T) {
	stopID := parkedStopID("agent", "u1", 0)
	router, st, probe := newTrayProbeRouter(t, stopID)
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Waiting on the agent"})
	probe.reset()
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	if queued := strings.Join(queuedRows(router, "t1"), ","); queued != stopID {
		t.Fatalf("queued rows = %q, want the parked stop behind the main stream", queued)
	}
	if state, _ := probe.served("agent"); state == store.AgentRunParked {
		t.Fatal("the tray was served the agent parked before its stop landed")
	}

	probe.reset()
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
	router.WaitForPendingSettles()
	if got := parkedStops(t, st, "t1", "agent"); len(got) != 1 {
		t.Fatalf("the queued parked stop did not land: %v", got)
	}
	if state, withRow := probe.served("agent"); state != store.AgentRunParked || !withRow {
		t.Fatalf("after the drain the tray serves the agent %q (row written: %v), want parked from its landed stop", state, withRow)
	}
}

// The wake announces its agent once its row is written, serving it
// running, and nudges the gates that read the stash it dropped once.
func TestWakeAnnouncesTheAgentAfterItsRow(t *testing.T) {
	wakeID := provider.SubagentWakePromptItemID("shell")
	router, st, probe := newTrayProbeRouter(t, wakeID)
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")

	probe.reset()
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	if state, withRow := probe.served("agent"); state != store.AgentRunRunning || !withRow {
		t.Fatalf("the wake served the agent %q (row written: %v), want running after its row", state, withRow)
	}
	if probe.nudges != 1 {
		t.Errorf("the wake sent %d background_tasks_changed nudges, want 1 for the stash it dropped", probe.nudges)
	}
	if !strings.HasPrefix(mustGetItem(t, st, "t1", wakeID).Summary, "Background command") {
		t.Errorf("wake row summary = %q", mustGetItem(t, st, "t1", wakeID).Summary)
	}
}
