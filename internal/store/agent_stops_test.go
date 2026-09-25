package store

import (
	"slices"
	"testing"
)

// seedParkedStop writes a parked sibling of launchID at item index idx.
func seedParkedStop(t *testing.T, s *Store, threadID, id, launchID string, idx int, createdAt int64) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID: id, ThreadID: threadID, ItemIndex: idx, Kind: "tool_completion", Role: "assistant",
		Status: ItemStatusParked, Summary: id, IsBackground: true, CompletionOf: launchID,
		Meta: `{"task_id":"task-1"}`, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("seed parked stop %s: %v", id, err)
	}
}

// seedAgentWake writes the parser's wake row under rootID.
func seedAgentWake(t *testing.T, s *Store, threadID, id, rootID string, idx int, createdAt int64) {
	t.Helper()
	if err := insertCarded(s, Item{
		ID: id, ThreadID: threadID, ItemIndex: idx, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "shell done", ParentID: rootID, Meta: `{"wire_only":true,"subagent_wake_prompt":true,"task_id":"task-1"}`,
		CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
}

func liveTaskIDs(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	items, err := s.ListLiveBackgroundTasks(threadID, 0)
	if err != nil {
		t.Fatalf("live tasks: %v", err)
	}
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// A parked sibling is a pause: no trigger settles its launch on it, and
// removing it revives nothing. Only the ending sibling settles, and the
// readers that ask "has this launch settled" answer from the ending one.
func TestParkedStopSettlesNothing(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{"task_id":"task-1"}`)
	seedParkedStop(t, s, "t", "parked-1", "launch", 1, 2000)
	assertLive(t, s, "t", "launch")

	// Every reader of the settled question still counts the launch live.
	if live, err := s.HasLiveBackgroundToolCall("t"); err != nil || !live {
		t.Errorf("HasLiveBackgroundToolCall = %v, %v; want true", live, err)
	}
	if n, err := s.CountLiveRunningBackgroundToolCalls("t"); err != nil || n != 1 {
		t.Errorf("CountLiveRunningBackgroundToolCalls = %d, %v; want 1", n, err)
	}
	recoverable, err := s.ListRecoverableClaudeBackgroundLaunchesForThread("t")
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != "launch" {
		t.Errorf("recoverable = %v, %v; want the parked launch", recoverable, err)
	}
	if ids := liveTaskIDs(t, s, "t"); !slices.Equal(ids, []string{"launch"}) {
		t.Errorf("live tasks = %v, want the launch alone (a parked stop is not a completion)", ids)
	}

	// A launch row written again over its parked sibling stays live.
	if _, err := s.UpsertItem(Item{
		ID: "launch", ThreadID: "t", Kind: "tool_call", Role: "assistant", Status: "running", Summary: "launch",
		IsBackground: true, Meta: `{"task_id":"task-1","description":"again"}`, CreatedAt: 1000,
	}, nil); err != nil {
		t.Fatalf("rewrite the launch: %v", err)
	}
	assertLive(t, s, "t", "launch")

	seedCompletionSibling(t, s, "t", "complete:launch", "launch", 2, 3000)
	assertSettled(t, s, "t", "launch")
	if ids := liveTaskIDs(t, s, "t"); !slices.Equal(ids, []string{"launch", "complete:launch"}) {
		t.Errorf("live tasks = %v, want the settled launch with its ending sibling", ids)
	}

	// Deleting the parked sibling revives nothing; deleting the ending
	// one does, though a parked sibling remains.
	seedParkedStop(t, s, "t", "parked-2", "launch", 3, 2500)
	if err := s.DeleteThreadItem("t", "parked-2"); err != nil {
		t.Fatalf("delete parked: %v", err)
	}
	assertSettled(t, s, "t", "launch")
	if err := s.DeleteThreadItem("t", "complete:launch"); err != nil {
		t.Fatalf("delete completion: %v", err)
	}
	assertLive(t, s, "t", "launch")
}

// A parked sibling records one run and borrows no card, so its insert
// leaves its launch's rev alone, and no write under the launch restamps
// it: neither the history triggers at the child's insert nor the
// subagent_aggregates stamp at its card's flush. An ending sibling
// settles its launch, which moves the launch's rev, and follows the
// launch's card.
func TestParkedStopIsStampedOnlyByItsOwnWrites(t *testing.T) {
	s := settleTriggerStore(t)
	for i, id := range []string{"parked", "ended"} {
		if err := s.InsertItem(Item{ID: id, ThreadID: "t", ItemIndex: i, Kind: "tool_call", Role: "assistant",
			Status: "running", Summary: id, ToolName: "Agent", IsBackground: true, Meta: `{"task_id":"task-` + id + `"}`,
			CreatedAt: 1000}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	before := itemRevisionOf(t, s, "t", "parked")
	seedParkedStop(t, s, "t", "parked-stop", "parked", 2, 2000)
	if got := itemRevisionOf(t, s, "t", "parked"); got != before {
		t.Errorf("a parked sibling insert moved its launch's rev from %d to %d", before, got)
	}
	before = itemRevisionOf(t, s, "t", "ended")
	seedCompletionSibling(t, s, "t", "ended-stop", "ended", 3, 2000)
	if got := itemRevisionOf(t, s, "t", "ended"); got == before {
		t.Errorf("an ending sibling insert left its launch's rev at %d", got)
	}

	parkedRev, endedRev := itemRevisionOf(t, s, "t", "parked-stop"), itemRevisionOf(t, s, "t", "ended-stop")
	for i, parent := range []string{"parked", "ended"} {
		if err := insertCarded(s, Item{ID: parent + "-child", ThreadID: "t", ItemIndex: 4 + i, Kind: "tool_call",
			Role: "assistant", Status: "completed", Summary: "Read", ToolName: "Read", ParentID: parent, CreatedAt: 3000}); err != nil {
			t.Fatalf("seed a child of %s: %v", parent, err)
		}
	}
	if got := itemRevisionOf(t, s, "t", "parked-stop"); got != parkedRev {
		t.Errorf("a write under the launch restamped its parked sibling from %d to %d", parkedRev, got)
	}
	if got := itemRevisionOf(t, s, "t", "ended-stop"); got == endedRev {
		t.Errorf("a write under the launch left its ending sibling at %d", got)
	}
}

// A launch whose parked sibling was written before the launch row stays
// live: the launch-insert trigger counts only an ending sibling.
func TestLaunchInsertedAfterItsParkedStopStaysLive(t *testing.T) {
	s := settleTriggerStore(t)
	seedParkedStop(t, s, "t", "parked-1", "launch", 1, 2000)
	seedLaunchWithMeta(t, s, "t", "launch", 0, `{"task_id":"task-1"}`)
	assertLive(t, s, "t", "launch")
}

// The launch is parked at its newest stop until a wake under its root at
// or after that stop; an ending stop, or no stop, is not parked.
func TestCurrentParkedStopFollowsStopsAndWakes(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "root", 0, `{"task_id":"task-1"}`)
	parked := func(launch, root string) string {
		t.Helper()
		stop, found, err := s.CurrentParkedStop("t", launch, root)
		if err != nil {
			t.Fatalf("current parked stop: %v", err)
		}
		if !found {
			return ""
		}
		return stop.ID
	}
	if got := parked("root", "root"); got != "" {
		t.Fatalf("a launch with no stop is parked at %q", got)
	}
	seedParkedStop(t, s, "t", "parked-1", "root", 1, 2000)
	if got := parked("root", "root"); got != "parked-1" {
		t.Fatalf("parked at %q, want parked-1", got)
	}
	// A wake before the stop belongs to an earlier run.
	seedAgentWake(t, s, "t", "wake-0", "root", 2, 1500)
	if got := parked("root", "root"); got != "parked-1" {
		t.Fatalf("an earlier wake unparked the agent: %q", got)
	}
	// A wake in the stop's own millisecond follows it.
	seedAgentWake(t, s, "t", "wake-1", "root", 3, 2000)
	if got := parked("root", "root"); got != "" {
		t.Fatalf("the woken agent is parked at %q", got)
	}
	if at, found, err := s.LatestAgentWake("t", "root", 1600); err != nil || !found || at != 2000 {
		t.Fatalf("latest wake since 1600 = %d found=%v err=%v, want 2000", at, found, err)
	}
	if _, found, err := s.LatestAgentWake("t", "root", 2001); err != nil || found {
		t.Fatalf("a wake after 2001: found=%v err=%v", found, err)
	}
	seedParkedStop(t, s, "t", "parked-2", "root", 4, 3000)
	if got := parked("root", "root"); got != "parked-2" {
		t.Fatalf("parked at %q, want the newer parked-2", got)
	}
	// A carrier's wakes are filed under the root too.
	if got := parked("root", "other-root"); got != "parked-2" {
		t.Fatalf("a wake under another root unparked the agent: %q", got)
	}
	seedCompletionSibling(t, s, "t", "complete:root", "root", 5, 4000)
	if got := parked("root", "root"); got != "" {
		t.Fatalf("an ended agent is parked at %q", got)
	}
}

// A task's newest stop is read across every row it was bound to, and only
// from its stops: its wake rows and bells carry the task id too.
func TestNewestTaskStopReadsEveryRowOfTheTask(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "root", 0, `{"task_id":"task-1"}`)
	seedLaunchWithMeta(t, s, "t", "carrier", 1, `{"task_id":"task-1"}`)
	seedParkedStop(t, s, "t", "root-parked", "root", 2, 2000)
	seedAgentWake(t, s, "t", "wake-1", "root", 3, 2500)
	seedParkedStop(t, s, "t", "carrier-parked", "carrier", 4, 3000)
	for _, row := range []Item{
		{ID: "bell", Kind: "notification", Role: "system", ItemIndex: 5, Meta: `{"task_id":"task-1"}`, CreatedAt: 3500, UpdatedAt: 3500},
		{ID: "other-stop", Kind: "tool_completion", Role: "assistant", ItemIndex: 6, CompletionOf: "root",
			IsBackground: true, Status: ItemStatusParked, Meta: `{"task_id":"task-2"}`, CreatedAt: 4000},
	} {
		row.ThreadID, row.Summary = "t", row.ID
		if err := s.InsertItem(row); err != nil {
			t.Fatalf("seed %s: %v", row.ID, err)
		}
	}

	stop, found, err := s.NewestTaskStop("t", "task-1")
	if err != nil || !found || stop.ID != "carrier-parked" {
		t.Fatalf("newest stop of task-1 = %q found=%v err=%v, want carrier-parked", stop.ID, found, err)
	}
	if _, found, err := s.NewestTaskStop("t", ""); err != nil || found {
		t.Fatalf("newest stop of no task: found=%v err=%v", found, err)
	}
	if launch, found, err := s.FindToolCallItemByTaskID("t", "task-1"); err != nil || !found || launch.Kind != "tool_call" {
		t.Fatalf("tool call of task-1 = %q (%s) found=%v err=%v, want a tool call past the newer stops, wake and bell", launch.ID, launch.Kind, found, err)
	}
}

// A §E6 rebind retires the rows the task was bound to that are parked; the
// carrier, a settled row and a row that never parked are left alone.
func TestRetireParkedAgentLaunches(t *testing.T) {
	s := settleTriggerStore(t)
	seedLaunchWithMeta(t, s, "t", "root", 0, `{"task_id":"task-1"}`)
	seedParkedStop(t, s, "t", "parked-root", "root", 1, 2000)
	seedLaunchWithMeta(t, s, "t", "carrier", 2, `{"task_id":"task-1","transcript_root_id":"root"}`)
	seedParkedStop(t, s, "t", "parked-carrier", "carrier", 3, 2500)
	seedLaunchWithMeta(t, s, "t", "never-parked", 4, `{"task_id":"task-1"}`)
	seedLaunchWithMeta(t, s, "t", "ended", 5, `{"task_id":"task-1"}`)
	seedParkedStop(t, s, "t", "parked-ended", "ended", 6, 2600)
	seedCompletionSibling(t, s, "t", "complete:ended", "ended", 7, 2700)
	seedLaunchWithMeta(t, s, "t", "other-task", 8, `{"task_id":"task-2"}`)
	seedParkedStop(t, s, "t", "parked-other", "other-task", 9, 2800)

	retired, err := s.RetireParkedAgentLaunches("t", "task-1", "carrier")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if !slices.Equal(retired, []string{"root"}) {
		t.Fatalf("retired %v, want the parked root alone", retired)
	}
	assertSettled(t, s, "t", "root")
	for _, id := range []string{"carrier", "never-parked", "other-task"} {
		assertLive(t, s, "t", id)
	}
	if again, err := s.RetireParkedAgentLaunches("t", "task-1", "carrier"); err != nil || len(again) != 0 {
		t.Fatalf("a second retire = %v, %v; want nothing", again, err)
	}
	if none, err := s.RetireParkedAgentLaunches("t", "", "carrier"); err != nil || len(none) != 0 {
		t.Fatalf("a retire with no task = %v, %v", none, err)
	}
	recoverable, err := s.ListRecoverableClaudeBackgroundLaunchesForThread("t")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range recoverable {
		if item.ID == "root" {
			t.Error("the retired root is still recoverable at session end")
		}
	}
}
