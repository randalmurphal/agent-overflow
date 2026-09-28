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

// stopReportShown is the answer a stop's row shows on its card, "" for
// none: the report head when the row says its notification loaded
// (showsAgentStopReport), as the frontend reads it.
func stopReportShown(t *testing.T, row store.Item) string {
	t.Helper()
	if !showsAgentStopReport(row, nil) {
		return ""
	}
	var meta struct {
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(row.PayloadMeta), &meta); err != nil {
		t.Fatalf("decode the payload meta of %s: %v", row.ID, err)
	}
	return meta.Preview
}

// stopReportPushes lists the answer each pushed write of row id showed.
func stopReportPushes(t *testing.T, events []emitted, id string) []string {
	t.Helper()
	var shown []string
	for _, row := range itemUpserts(events) {
		if row.ID == id {
			shown = append(shown, stopReportShown(t, row))
		}
	}
	return shown
}

// assertStopShows checks that the stored ending sibling of launchID, and
// every push of it in events, shows report: its card mounts with its
// answer and never changes it.
func assertStopShows(t *testing.T, st *store.Store, events []emitted, launchID, report string) {
	t.Helper()
	id := ToolCompletionID(launchID)
	pushes := stopReportPushes(t, events, id)
	if len(pushes) == 0 {
		t.Fatalf("%s was never pushed", id)
	}
	for i, shown := range pushes {
		if shown != report {
			t.Fatalf("push %d of %s showed %q, want %q (all pushes: %q)", i, id, shown, report, pushes)
		}
	}
	if shown := stopReportShown(t, mustGetItem(t, st, "t1", id)); shown != report {
		t.Fatalf("stored %s shows %q, want %q", id, shown, report)
	}
}

// An agent's ending sibling lands with its report in its first write:
// no push of it shows the card without its answer.
func TestAgentStopLandsWithItsReport(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkTerminal(t, router, "t1", "agent", "task-agent")
	emissions.reset()
	parkNotify(t, router, "t1", "agent", "task-agent", "The final report.", "u1")
	assertStopShows(t, st, emissions.snapshot(), "agent", "The final report.")
}

// A stop that arrives while the main agent streams waits behind the
// stream, and lands with its report when the stream ends. The report
// used to be attached by a second write that looked only in the store,
// missed the queued row and was dropped.
func TestAgentStopQueuedBehindAStreamKeepsItsReport(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is talking"})
	parkTerminal(t, router, "t1", "agent", "task-agent")
	emissions.reset()
	parkNotify(t, router, "t1", "agent", "task-agent", "The final report.", "u1")
	if queued := strings.Join(queuedRows(router, "t1"), ","); queued != ToolCompletionID("agent") {
		t.Fatalf("queued rows = %q, want the stop behind the main stream", queued)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
	router.WaitForPendingSettles()
	assertStopShows(t, st, emissions.snapshot(), "agent", "The final report.")
}

// The CLI hands a stop to the model again at its next tool round, and
// that notice carries only the `Agent "…" finished` bell (or the report
// again). A later notice of a recorded stop never replaces or clears the
// report, and writes nothing while there is nothing to add, whether the
// stop is stored or still queued behind a stream.
func TestLaterNoticeOfAnAgentStopKeepsItsReport(t *testing.T) {
	bell := `Agent "Spike agent" finished`
	t.Run("stored", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
		parkTerminal(t, router, "t1", "agent", "task-agent")
		parkNotify(t, router, "t1", "agent", "task-agent", "The final report.", "u1")
		stored := mustGetItem(t, st, "t1", ToolCompletionID("agent"))

		emissions.reset()
		parkNotify(t, router, "t1", "agent", "task-agent", bell, "u2")
		parkNotify(t, router, "t1", "agent", "task-agent", "A different report.", "u3")
		if pushes := stopReportPushes(t, emissions.snapshot(), ToolCompletionID("agent")); len(pushes) != 0 {
			t.Fatalf("later notices rewrote the stop: pushes %q", pushes)
		}
		again := mustGetItem(t, st, "t1", ToolCompletionID("agent"))
		if again.Meta != stored.Meta || again.PayloadID != stored.PayloadID || again.PayloadMeta != stored.PayloadMeta || again.UpdatedAt != stored.UpdatedAt {
			t.Fatalf("later notices changed the stop:\n%+v\n%+v", stored, again)
		}
	})
	// A host terminal stashed after the stop was written merges into the
	// sibling again with the next notice, which still keeps the report.
	t.Run("stashed after the stop", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
		parkNotify(t, router, "t1", "agent", "task-agent", "The final report.", "u1")
		parkTerminal(t, router, "t1", "agent", "task-agent")
		parkNotify(t, router, "t1", "agent", "task-agent", bell, "u2")
		if shown := stopReportShown(t, mustGetItem(t, st, "t1", ToolCompletionID("agent"))); shown != "The final report." {
			t.Fatalf("stop shows %q, want the report it was written with", shown)
		}
	})
	t.Run("queued", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is talking"})
		parkTerminal(t, router, "t1", "agent", "task-agent")
		emissions.reset()
		parkNotify(t, router, "t1", "agent", "task-agent", "The final report.", "u1")
		parkNotify(t, router, "t1", "agent", "task-agent", bell, "u2")
		parkNotify(t, router, "t1", "agent", "task-agent", "A different report.", "u3")
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
		router.WaitForPendingSettles()
		assertStopShows(t, st, emissions.snapshot(), "agent", "The final report.")
	})
}

// A stop another signal recorded first has no report yet: a kill's
// task_updated writes the ending sibling before the notification. The
// notification gives it the report, stored or still queued behind a
// stream, and a later notice keeps it.
func TestNoticeGivesAKilledStopItsReport(t *testing.T) {
	kill := func(t *testing.T, router *Router) {
		t.Helper()
		parkHandle(t, router, provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent",
			Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated"}),
		})
	}
	stopped := func(t *testing.T, router *Router, summary, uuid string) {
		t.Helper()
		parkHandle(t, router, provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: "agent", Content: summary,
			Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "stopped", "uuid": uuid, "output_file": "/tmp/agent-task-agent.output"}),
		})
	}
	t.Run("stored", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
		kill(t, router)
		if shown := stopReportShown(t, mustGetItem(t, st, "t1", ToolCompletionID("agent"))); shown != "" {
			t.Fatalf("the kill's sibling shows %q before any notification", shown)
		}
		emissions.reset()
		stopped(t, router, "Spike agent", "u0")
		stopped(t, router, `Agent "Spike agent" was stopped by user`, "u0b")
		if pushes := stopReportPushes(t, emissions.snapshot(), ToolCompletionID("agent")); len(pushes) != 0 {
			t.Fatalf("a notice without a report rewrote the stop: pushes %q", pushes)
		}
		stopped(t, router, "Partial findings.", "u1")
		emissions.reset()
		stopped(t, router, `Agent "Spike agent" was stopped`, "u2")
		if pushes := stopReportPushes(t, emissions.snapshot(), ToolCompletionID("agent")); len(pushes) != 0 {
			t.Fatalf("a later notice rewrote the stop: pushes %q", pushes)
		}
		if shown := stopReportShown(t, mustGetItem(t, st, "t1", ToolCompletionID("agent"))); shown != "Partial findings." {
			t.Fatalf("stop shows %q, want the notification's report", shown)
		}
		if status := mustGetItem(t, st, "t1", ToolCompletionID("agent")).Status; status != statusKilled {
			t.Fatalf("stop status = %q, want %q", status, statusKilled)
		}
	})
	t.Run("queued", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is talking"})
		emissions.reset()
		kill(t, router)
		stopped(t, router, "Partial findings.", "u1")
		if queued := strings.Join(queuedRows(router, "t1"), ","); queued != ToolCompletionID("agent") {
			t.Fatalf("queued rows = %q, want the one stop behind the main stream", queued)
		}
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
		router.WaitForPendingSettles()
		assertStopShows(t, st, emissions.snapshot(), "agent", "Partial findings.")
	})
}

// A notification's summary is an agent's report unless it is status: the
// CLI's detail-free outcome line, the bare description a kill's
// envelope carries, or the summary-less placeholder.
func TestAgentReportFromNotification(t *testing.T) {
	launch := store.Item{Meta: `{"toolName":"Agent","input":{"description":"Spike agent","prompt":"go"}}`}
	for _, tc := range []struct{ summary, want string }{
		{"The final report.", "The final report."},
		{`Agent "Spike agent" failed: boom`, `Agent "Spike agent" failed: boom`},
		{`Agent "Spike agent" was stopped: boom`, `Agent "Spike agent" was stopped: boom`},
		{`Agent "Other agent" finished`, `Agent "Other agent" finished`},
		{`Agent "Spike agent" finished`, ""},
		{`Agent "Spike agent" was stopped`, ""},
		{`Agent "Spike agent" was stopped by user`, ""},
		{`Agent "Spike agent" was stopped by Claude`, ""},
		{"Spike agent", ""},
		{backgroundTaskNotificationPlaceholderSummary, ""},
		{"  ", ""},
	} {
		if got := agentReportFromNotification(launch, tc.summary); got != tc.want {
			t.Errorf("agentReportFromNotification(%q) = %q, want %q", tc.summary, got, tc.want)
		}
	}
}

// A nested async agent is a task of the main session, so it can outlive
// the agent that launched it (claude-wire.md fixture D). That agent's last
// card is a snapshot up to its own stop, so a later stop of the child
// files at top level, where its card can show; while the parent still
// runs or is parked, its next card holds the child's stop.
func TestNestedAgentStopFilesUnderItsParentOnlyWhileTheParentRuns(t *testing.T) {
	launch := func(t *testing.T) (*Router, *store.Store) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		parkLaunchAgent(t, router, "t1", "parent", "task-parent", "")
		parkLaunchAgent(t, router, "t1", "child", "task-child", "parent")
		return router, st
	}
	childStopScope := func(t *testing.T, st *store.Store) string {
		t.Helper()
		stop, ok := parkCompletions(t, st, "t1")["child"]
		if !ok {
			t.Fatal("the child's stop was not written")
		}
		return stop.ParentID
	}

	t.Run("parent running", func(t *testing.T) {
		router, st := launch(t)
		parkStop(t, router, "t1", "child", "task-child", "CHILD DONE", "u-child")
		if got := childStopScope(t, st); got != "parent" {
			t.Fatalf("child stop scope = %q, want the running parent", got)
		}
	})
	t.Run("parent parked", func(t *testing.T) {
		router, st := launch(t)
		parkLaunchShell(t, router, "t1", "shell", "task-shell", "parent")
		parkStop(t, router, "t1", "parent", "task-parent", "WAITING", "u-parent")
		if len(parkedStops(t, st, "t1", "parent")) != 1 {
			t.Fatal("the parent must park on its shell")
		}
		parkStop(t, router, "t1", "child", "task-child", "CHILD DONE", "u-child")
		if got := childStopScope(t, st); got != "parent" {
			t.Fatalf("child stop scope = %q, want the parked parent, whose next card holds it", got)
		}
	})
	t.Run("parent ended", func(t *testing.T) {
		router, st := launch(t)
		parkStop(t, router, "t1", "parent", "task-parent", "PARENT DONE", "u-parent")
		parkStop(t, router, "t1", "child", "task-child", "CHILD DONE", "u-child")
		if got := childStopScope(t, st); got != "" {
			t.Fatalf("child stop scope = %q, want top level after the parent ended", got)
		}
	})
	t.Run("parent end queued behind a stream", func(t *testing.T) {
		router, st := launch(t)
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is talking"})
		parkStop(t, router, "t1", "parent", "task-parent", "PARENT DONE", "u-parent")
		parkStop(t, router, "t1", "child", "task-child", "CHILD DONE", "u-child")
		parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
		router.WaitForPendingSettles()
		if got := childStopScope(t, st); got != "" {
			t.Fatalf("child stop scope = %q, want top level: the parent's end was already queued", got)
		}
	})
	t.Run("parked child after the parent ended", func(t *testing.T) {
		router, st := launch(t)
		parkStop(t, router, "t1", "parent", "task-parent", "PARENT DONE", "u-parent")
		parkLaunchShell(t, router, "t1", "child-shell", "task-child-shell", "child")
		parkStop(t, router, "t1", "child", "task-child", "CHILD WAITING", "u-child")
		stops := parkedStops(t, st, "t1", "child")
		if len(stops) != 1 || stops[0].ParentID != "" {
			t.Fatalf("child parked stops = %+v, want one at top level", stops)
		}
	})
}
