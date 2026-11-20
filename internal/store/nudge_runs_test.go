package store

import "testing"

// StartNudgeRun records a run of the spawner's only for an owned worker at
// rest to it, whose spawner is not archived and is not the sender: the row
// is a settled, unowed send of the spawner's with its own run open, which
// thread_status does not list and a later request of the spawner's joins.
// DropNudgeRun removes one nobody joined; a sidebar row ends an open one.
func TestANudgeRunIsRecordedOnlyForAnOwnedWorkerAtRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "w"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "spawn", "lead", ThreadRequestSpawn, "w", false, 100)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	start := func(token, by string) bool {
		t.Helper()
		started, err := s.StartNudgeRun(token, "w", by)
		if err != nil {
			t.Fatalf("StartNudgeRun(%s): %v", token, err)
		}
		return started
	}
	if start("during-spawn", "") {
		t.Fatal("a nudge during the spawn's run recorded a run of its own")
	}
	settleAgentRequest(t, s, "spawn", 200)
	mustExec(t, s.db, `UPDATE thread_requests SET run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = 'spawn'`)

	if start("by-spawner", "lead") {
		t.Fatal("the spawner's own message recorded a nudge")
	}
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if start("sidebar", "") {
		t.Fatal("a worker with a sidebar row recorded a nudge")
	}
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE threads SET archived = 1 WHERE id = 'lead'`)
	if start("archived", "") {
		t.Fatal("a worker of an archived spawner recorded a nudge")
	}
	mustExec(t, s.db, `UPDATE threads SET archived = 0 WHERE id = 'lead'`)

	if !start("dropped", "") {
		t.Fatal("the nudge recorded no run")
	}
	if err := s.DropNudgeRun("dropped"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadRequest("dropped"); err != nil || found {
		t.Fatalf("the dropped nudge is found %v, %v", found, err)
	}

	if !start("nudge", "") {
		t.Fatal("the nudge recorded no run")
	}
	row := rawThreadRequest(t, s, "nudge")
	if !row.Nudge || row.CallerThreadID != "lead" || row.Kind != ThreadRequestSend || row.State != ThreadRequestFinished ||
		row.SettledAt == 0 || row.Notify || !row.RunOpen() || row.RunToken != "nudge" {
		t.Fatalf("nudge %+v; want a settled, unowed send of the spawner's with its own run open", row)
	}
	if listed, err := s.ListThreadRequestsByCaller("lead", 10, 0); err != nil || len(listed) != 1 || listed[0].Token != "spawn" {
		t.Fatalf("ListThreadRequestsByCaller = %+v, %v; want the spawn alone", listed, err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "joined", CallerThreadID: "lead", Kind: ThreadRequestSend, TargetThreadID: "w",
		State: ThreadRequestRunning, CreatedAt: nowMillis(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := rawThreadRequest(t, s, "joined").RunToken; got != "nudge" {
		t.Fatalf("the spawner's send during the nudge's run is in run %q; want the nudge's", got)
	}
	if err := s.DropNudgeRun("nudge"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadRequest("nudge"); err != nil || !found {
		t.Fatalf("the nudge a send joined is found %v, %v; want it kept", found, err)
	}

	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if !rawThreadRequest(t, s, "nudge").RunEnded() {
		t.Fatal("a sidebar row left the nudge's run open")
	}
	if rawThreadRequest(t, s, "joined").RunEnded() {
		t.Fatal("a sidebar row ended the run of an unsettled request")
	}
}

// Grouping an owned worker gives it a sidebar row, which ends its open
// nudge run as Show in Sidebar does.
func TestGroupingAnOwnedWorkerEndsItsNudgeRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "w"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "spawn", "lead", ThreadRequestSpawn, "w", false, 100)
	settleAgentRequest(t, s, "spawn", 200)
	mustExec(t, s.db, `UPDATE thread_requests SET run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = 'spawn'`)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartNudgeRun("nudge", "w", ""); err != nil || !started {
		t.Fatalf("StartNudgeRun = %v, %v", started, err)
	}
	thread, err := s.GetThread("w")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateThreadGroup(thread.ProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"w"}, group.ID); err != nil {
		t.Fatal(err)
	}
	if !rawThreadRequest(t, s, "nudge").RunEnded() {
		t.Fatal("grouping the worker left the nudge's run open")
	}
}
