package triage

// The background tray's deltas (background_tray.go): the writes that move
// a row the tray serves announce its launch on provider:background_tray
// with the launch's rows as the tray reads them, and nothing else does.

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func trayFrames(emissions *emissionLog) []BackgroundTrayEvent {
	var out []BackgroundTrayEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != eventchan.ProviderBackgroundTray.String() {
			continue
		}
		out = append(out, e.data.(BackgroundTrayEvent))
	}
	return out
}

// trayRow is the row a delta carries for id, if any.
func trayRow(frame BackgroundTrayEvent, id string) (store.Item, bool) {
	for _, row := range frame.Rows {
		if row.ID == id {
			return row, true
		}
	}
	return store.Item{}, false
}

// namesOnly checks that a delta answers for exactly the launches named.
func namesOnly(t *testing.T, what string, frame BackgroundTrayEvent, ids ...string) {
	t.Helper()
	if frame.Refresh || !slices.Equal(frame.LaunchIDs, ids) {
		t.Fatalf("%s: frame %+v, want a delta naming %v", what, frame, ids)
	}
}

func trayChildTool(t *testing.T, router *Router, threadID, id, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: id, ItemType: "Read", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Read", "input": map[string]any{"file_path": "/tmp/" + id}}),
	})
}

func trayChildDone(t *testing.T, router *Router, threadID, id, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: id, ParentToolUseID: parentID,
		Content: "done", Meta: parkMeta(t, map[string]any{"exit_code": 0}),
	})
}

// A background launch's own writes carry its row; its completion names the
// launch alone: a parked agent serves the count its stop recorded, so the
// agent a command settles under is not read again.
func TestBackgroundLaunchAndCompletionCarryTheirRows(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")

	emissions.reset()
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	frames := trayFrames(emissions)
	if len(frames) == 0 {
		t.Fatal("the shell's launch sent no tray frame")
	}
	last := frames[len(frames)-1]
	namesOnly(t, "shell launch", last, "shell")
	if row, ok := trayRow(last, "shell"); !ok || row.Status != statusRunning || !row.IsBackground {
		t.Fatalf("shell launch carried %+v, want the running shell row", last.Rows)
	}

	emissions.reset()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	var done *BackgroundTrayEvent
	for _, frame := range trayFrames(emissions) {
		if slices.Contains(frame.LaunchIDs, "shell") {
			done = &frame
		}
	}
	if done == nil {
		t.Fatal("the shell's completion sent no tray frame naming it")
	}
	namesOnly(t, "shell completion", *done, "shell")
	completion := false
	for _, row := range done.Rows {
		completion = completion || row.CompletionOf == "shell"
	}
	if !completion {
		t.Fatalf("shell completion carried %+v, want its completion sibling", done.Rows)
	}
}

// A nested agent joins the tray at its first child and its latest-tool
// line moves at the refresh that restamps it; both pushes are scoped to its
// parent, so the tray hears of them on its own channel. A top-level
// agent's own push reaches the tray, so its restamp sends nothing here.
func TestNestedLaunchAnnouncedAtItsFirstChildAndRestamp(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "outer", "task-outer", "")
	parkLaunchAgent(t, router, "t1", "nested", "task-nested", "outer")
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c1", "nested")
	frames := trayFrames(emissions)
	if len(frames) != 1 {
		t.Fatalf("the nested agent's first child sent %d tray frames, want 1", len(frames))
	}
	namesOnly(t, "first child", frames[0], "nested")
	if _, ok := trayRow(frames[0], "nested"); !ok {
		t.Fatalf("first child carried %+v, want the nested agent's row", frames[0].Rows)
	}
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c2", "nested")
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Fatalf("a child write sent %d tray frames before the refresh, want 0", len(frames))
	}
	router.DrainWireItemRefresh()
	frames = trayFrames(emissions)
	if len(frames) != 1 {
		t.Fatalf("the refresh that restamped the nested agent sent %d tray frames, want 1", len(frames))
	}
	namesOnly(t, "restamp", frames[0], "nested")
	if row, _ := trayRow(frames[0], "nested"); !json.Valid([]byte(row.Meta)) || row.ID != "nested" {
		t.Fatalf("restamp carried %+v, want the nested agent's row", frames[0].Rows)
	}

	emissions.reset()
	trayChildTool(t, router, "t1", "c3", "outer")
	router.DrainWireItemRefresh()
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Errorf("a top-level agent's restamp sent %d tray frames, want 0", len(frames))
	}
}

// A settled nested agent is not in the live list: its restamp announces
// nothing.
func TestSettledNestedLaunchRestampSendsNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "outer", "task-outer", "")
	parkLaunchAgent(t, router, "t1", "nested", "task-nested", "outer")
	trayChildTool(t, router, "t1", "c1", "nested")
	parkStop(t, router, "t1", "nested", "task-nested", "done", "u1")
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c2", "nested")
	router.DrainWireItemRefresh()
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Errorf("a settled nested agent's restamp sent %d tray frames, want 0", len(frames))
	}
}

// A foreground agent under a background one is in the tray while it runs
// (class 2). Its settle names it, and the delta carries no row for it: it
// left. A plain call settling under an agent anchors nothing, so it sends
// no frame (TestSubagentToolEventReadsItsRowOnce pins that it reads
// nothing for the tray either).
func TestNestedSettleAnnouncesOnlyAnAgentLaunch(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "outer", "task-outer", "")
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "inner", ItemType: "Agent", ParentToolUseID: "outer",
		Meta: parkMeta(t, map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Inner", "prompt": "p"}}),
	})
	trayChildTool(t, router, "t1", "inner-read", "inner")
	trayChildTool(t, router, "t1", "outer-read", "outer")
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(live, func(it store.Item) bool { return it.ID == "inner" }) {
		t.Fatalf("the running inner agent is not a tray row: %v", pushedIDs(live))
	}

	emissions.reset()
	trayChildDone(t, router, "t1", "outer-read", "outer")
	trayChildDone(t, router, "t1", "inner-read", "inner")
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Fatalf("plain calls settling sent %+v, want no tray frame", frames)
	}

	emissions.reset()
	trayChildDone(t, router, "t1", "inner", "outer")
	frames := trayFrames(emissions)
	var settle *BackgroundTrayEvent
	for _, frame := range frames {
		if slices.Contains(frame.LaunchIDs, "inner") {
			settle = &frame
		}
	}
	if settle == nil {
		t.Fatalf("the inner agent's settle sent %+v, want a delta naming it", frames)
	}
	namesOnly(t, "inner settle", *settle, "inner")
	if row, ok := trayRow(*settle, "inner"); ok {
		t.Fatalf("the settled inner agent is still a tray row: %+v", row)
	}
}

// A park writes a parked sibling and a wake writes its row under the
// agent's root: each moves the agent's served run state, and each names the
// agent with its row.
func TestParkAndWakeAnnounceTheAgent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")

	stateAfter := func(what string) string {
		t.Helper()
		var state string
		for _, frame := range trayFrames(emissions) {
			if row, ok := trayRow(frame, "agent"); ok && slices.Contains(frame.LaunchIDs, "agent") {
				state = store.AgentRunState(row)
			}
		}
		if state == "" {
			t.Fatalf("%s sent no tray frame carrying the agent", what)
		}
		return state
	}

	emissions.reset()
	parkStop(t, router, "t1", "agent", "task-agent", "Waiting.", "u1")
	if state := stateAfter("the park"); state != store.AgentRunParked {
		t.Fatalf("the park's last frame served the agent %q, want %q", state, store.AgentRunParked)
	}

	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	emissions.reset()
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	if state := stateAfter("the wake"); state != store.AgentRunRunning {
		t.Fatalf("the wake's last frame served the agent %q, want %q", state, store.AgentRunRunning)
	}
}

// A §E6 rebind onto a parked agent retires the parked launch, which no push
// carries: the rebind names it, and the delta carries no row for it.
func TestRebindAnnouncesTheRetiredParkedAgent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	parkStop(t, router, "t1", "root", "task-agent", "ROUND 1", "u1")

	emissions.reset()
	resumeAgent(t, router, "t1", "carrier", map[string]any{
		"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root",
		"description": "Spike root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root",
	})
	var retired *BackgroundTrayEvent
	for _, frame := range trayFrames(emissions) {
		if slices.Contains(frame.LaunchIDs, "root") {
			retired = &frame
		}
	}
	if retired == nil {
		t.Fatalf("the rebind sent %+v, want a delta naming the retired root", trayFrames(emissions))
	}
	if row, ok := trayRow(*retired, "root"); ok {
		t.Fatalf("the retired root is still a tray row: %+v", row)
	}
}

// A delta whose read fails asks for the whole list instead of dropping the
// change.
func TestTrayDeltaReadFailureAsksForARefresh(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	emissions.reset()
	router.emitBackgroundTrayRows("t1", []string{"agent"})
	frames := trayFrames(emissions)
	if len(frames) != 1 || !frames[0].Refresh || len(frames[0].LaunchIDs) != 0 {
		t.Fatalf("a failed read sent %+v, want one refresh frame", frames)
	}
}

// A Codex thread's tray lists runtime records no row carries: a live
// agent's direct tool call asks for a refresh, as does a delta naming a
// launch (an anchor restamp), a background row's own push sends nothing
// (the tray takes it from the push), and a tool call under an agent that
// is no longer live sends nothing.
func TestCodexTrayFramesAreRefreshes(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	spawnMeta := buildSpawnAgentMeta(t, "child-1", "running")
	for _, kind := range []provider.EventKind{provider.EventToolStart, provider.EventToolComplete} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: kind, ThreadID: "t1", ItemID: "spawn-1", ItemType: "collab_agent", TurnID: "turn-0",
			Meta: spawnMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("spawn %s: %v", kind, err)
		}
	}
	if live := router.ListLiveCodexAgentTasks("t1"); len(live) != 1 {
		t.Fatalf("live Codex agents = %+v, want spawn-1", live)
	}

	emissions.reset()
	trayChildTool(t, router, "t1", "child-read", "spawn-1")
	frames := trayFrames(emissions)
	if len(frames) != 1 || !frames[0].Refresh {
		t.Errorf("a live agent's direct tool call sent %+v, want one refresh", frames)
	}
	emissions.reset()
	router.emitBackgroundTray("t1", "spawn-1")
	if frames := trayFrames(emissions); len(frames) != 1 || !frames[0].Refresh || len(frames[0].LaunchIDs) != 0 {
		t.Errorf("a delta naming a Codex launch sent %+v, want one refresh", frames)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-1",
		Meta: json.RawMessage(`{"agent_path":"child-1","status":"completed"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child completed: %v", err)
	}
	if live := router.ListLiveCodexAgentTasks("t1"); len(live) != 0 {
		t.Fatalf("live Codex agents after completion = %+v, want none", live)
	}
	emissions.reset()
	trayChildTool(t, router, "t1", "late-read", "spawn-1")
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Errorf("a tool call under a finished agent sent %+v, want nothing", frames)
	}

	emissions.reset()
	router.emitItemUpsert(store.Item{ID: "bg", ThreadID: "t1", Kind: itemKindToolCall, Status: statusRunning, IsBackground: true, Rev: store.UnstampedItemRev})
	if frames := trayFrames(emissions); len(frames) != 0 {
		t.Errorf("a Codex background row's push sent %+v, want nothing", frames)
	}
}

// A wake is a prompt row under a root whose meta sets the wake marker, as
// the store's wake probe reads it; any other row, or a meta that does not
// decode, announces nothing.
func TestIsWakePromptRow(t *testing.T) {
	marker := func(value string) string { return `{"` + provider.MetaSubagentWakePromptKey + `":` + value + `}` }
	for _, tc := range []struct {
		name string
		row  store.Item
		want bool
	}{
		{"a wake", store.Item{Kind: itemKindUserText, ParentID: "root", Meta: marker("true")}, true},
		{"the marker unset", store.Item{Kind: itemKindUserText, ParentID: "root", Meta: marker("false")}, false},
		{"a meta that does not decode", store.Item{Kind: itemKindUserText, ParentID: "root", Meta: marker("true")[1:]}, false},
		{"no root", store.Item{Kind: itemKindUserText, Meta: marker("true")}, false},
		{"not a prompt", store.Item{Kind: itemKindToolCall, ParentID: "root", Meta: marker("true")}, false},
	} {
		if got := isWakePromptRow(tc.row); got != tc.want {
			t.Errorf("%s: isWakePromptRow = %v, want %v", tc.name, got, tc.want)
		}
	}
}
