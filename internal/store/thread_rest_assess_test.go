package store

import (
	"fmt"
	"slices"
	"testing"
)

// everySpawnRests names every spawn past cutoff as read at rest: the
// sweep's answer on a computer whose threads all rest.
func everySpawnRests(s *Store, cutoff int64) RestedSpawns {
	return func() ([]string, error) {
		spawns, err := s.SpawnsPastFloor(cutoff)
		if err != nil {
			return nil, err
		}
		tokens := make([]string, 0, len(spawns))
		for _, spawn := range spawns {
			tokens = append(tokens, spawn.Token)
		}
		return tokens, nil
	}
}

// storeSourcesRest names the spawns past cutoff whose subtree's stored
// sources read at rest: what the app answers when nothing in memory keeps
// those threads busy (the app's rule, workerUnrest, also reads turns,
// queues, prompts, background work and timers).
func storeSourcesRest(s *Store, cutoff int64) RestedSpawns {
	return func() ([]string, error) {
		spawns, err := s.SpawnsPastFloor(cutoff)
		if err != nil {
			return nil, err
		}
		roots := make([]string, 0, len(spawns))
		for _, spawn := range spawns {
			roots = append(roots, spawn.ThreadID)
		}
		read, err := s.ReadSubtreeRest(roots, nil)
		if err != nil {
			return nil, err
		}
		var tokens []string
		for _, spawn := range spawns {
			rests := true
			for _, id := range read.Reached[spawn.ThreadID] {
				src := read.Sources[id]
				rests = rests && !src.Asking && !src.AnswerDue && !src.Workflow && !src.Wake && !src.Reminder && !src.OpenWork
			}
			if rests {
				tokens = append(tokens, spawn.Token)
			}
		}
		return tokens, nil
	}
}

// A spawn past the floor whose thread is here is cleared only when the
// caller names it as read at rest: a sweep told nothing, or told of
// another spawn, leaves it whole on both sides, and one told of it keeps
// only its lineage. Its thread's own state does not enter: the store does
// not read rest.
func TestTheSweepClearsNoWorkingSpawnUnlessTold(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root", "other"} {
		mustCreateThread(t, s, id)
	}
	spawnWithReceipt(t, s, "to-root", "lead", "root", 200)
	spawnWithReceipt(t, s, "to-other", "lead", "other", 200)
	spawns, err := s.SpawnsPastFloor(1000)
	if err != nil || !slices.Equal(spawns, []PastFloorSpawn{{"to-other", "other"}, {"to-root", "root"}}) {
		t.Fatalf("spawns past the floor = %+v, %v; want both, by token", spawns, err)
	}
	whole := func(what string) {
		t.Helper()
		if row, found, err := s.GetThreadRequest("to-root"); err != nil || !found || string(row.Answer) != "done" {
			t.Fatalf("the spawn after %s: %+v, found %v, %v; want it whole", what, row, found, err)
		}
		if receipt, found, err := s.GetThreadRequestReceipt("to-root"); err != nil || !found || string(receipt.Answer) != "done" {
			t.Fatalf("the receipt after %s: %+v, found %v, %v; want it whole", what, receipt, found, err)
		}
	}
	if _, _, err := s.DeleteThreadRequestsBefore(1000, nil); err != nil {
		t.Fatal(err)
	}
	whole("a sweep told nothing")
	if _, _, err := s.DeleteThreadRequestsBefore(1000, func() ([]string, error) { return []string{"to-other"}, nil }); err != nil {
		t.Fatal(err)
	}
	whole("a sweep told of another spawn")
	if row := rawThreadRequest(t, s, "to-other"); !row.LineageOnly {
		t.Fatalf("the spawn the sweep was told of: %+v; want its lineage", row)
	}
	if _, _, err := s.DeleteThreadRequestsBefore(1000, func() ([]string, error) { return nil, fmt.Errorf("unread") }); err == nil {
		t.Fatal("a sweep whose answer failed reported no error")
	}
	whole("a sweep whose answer failed")
	if _, _, err := s.DeleteThreadRequestsBefore(1000, func() ([]string, error) { return []string{"to-root"}, nil }); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "to-root"); !row.LineageOnly || row.Answer != nil {
		t.Fatalf("the spawn once told of it: %+v; want its lineage alone", row)
	}
	if _, found, err := s.GetThreadRequestReceipt("to-root"); err != nil || found {
		t.Fatalf("the receipt once told of it: found %v, %v; want its lineage alone", found, err)
	}
}

// The sweep reads what it may clear again with the writer held: a spawn
// its first answer named and its second does not stays whole.
func TestTheSweepWritesWhatItReadsWithTheWriterHeld(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root"} {
		mustCreateThread(t, s, id)
	}
	spawnWithReceipt(t, s, "to-root", "lead", "root", 200)
	calls := 0
	if _, _, err := s.DeleteThreadRequestsBefore(1000, func() ([]string, error) {
		calls++
		if calls == 1 {
			return []string{"to-root"}, nil
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("the sweep asked %d times; want before and inside its write", calls)
	}
	if row, found, err := s.GetThreadRequest("to-root"); err != nil || !found || string(row.Answer) != "done" {
		t.Fatalf("the spawn after a sweep whose second answer left it out: %+v, found %v, %v; want it whole", row, found, err)
	}
}

// The subtree read names each root's threads, the root and every thread
// local spawn links reach below it, settled links included, each once,
// and reads each one's sources as the rule reads them.
func TestASubtreeReadReachesThroughLinksAndReadsEachSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root", "a", "b", "c", "peer", "off"} {
		mustCreateThread(t, s, id)
	}
	link := func(token, caller, target string, at int64) {
		seedAgentRequest(t, s, token, caller, ThreadRequestSpawn, target, false, at)
		settleAgentRequest(t, s, token, at+1)
	}
	link("to-root", "lead", "root", 10)
	link("to-a", "root", "a", 20)
	link("to-b", "a", "b", 30)
	link("to-c", "root", "c", 40)
	// Off the subtree: lead's other work.
	link("to-off", "lead", "off", 60)
	seedAgentRequest(t, s, "open-off", "off", ThreadRequestSend, "peer", true, 70)

	// a's sources: an open send with no wake, one with its wake, an answer on
	// its way, a loop, a reminder.
	seedAgentRequest(t, s, "quiet-send", "a", ThreadRequestSend, "peer", false, 100)
	seedAgentRequest(t, s, "armed-send", "a", ThreadRequestSend, "peer", true, 101)
	seedAgentRequest(t, s, "owed", "a", ThreadRequestAsk, "peer", true, 102)
	settleAgentRequestOwed(t, s, "owed", 103)
	if _, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "a", Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 1 << 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remind", CallerThreadID: "a", Kind: ThreadRequestRemind, Answer: []byte("check the board"),
		State: ThreadRequestAccepted, DueAt: 1 << 50, CreatedAt: 104, UpdatedAt: 104}); err != nil {
		t.Fatal(err)
	}
	// c's: a workflow run bound to it, running; one rested whose report is
	// owed; and a spawn on another computer that reports open work.
	running := testWorkItem("run-c", "project-a", "running", 1)
	running.Goal, running.OriginThreadID = "ship it", "c"
	rested := testWorkItem("rested-b", "project-a", "done", 2)
	rested.Goal, rested.OriginThreadID = "review it", "b"
	for _, item := range []WorkItem{running, rested} {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remote", CallerThreadID: "c", Kind: ThreadRequestSpawn, TargetComputerID: "pc",
		TargetThreadID: "there", State: ThreadRequestFinished, NestedOpen: true, SettledAt: 120, CreatedAt: 110, UpdatedAt: 120}); err != nil {
		t.Fatal(err)
	}

	read, err := s.ReadSubtreeRest([]string{"root", "b"}, []string{"rested-b"})
	if err != nil {
		t.Fatal(err)
	}
	for root, want := range map[string][]string{"root": {"a", "b", "c", "root"}, "b": {"b"}} {
		got := slices.Clone(read.Reached[root])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("reached from %s = %v; want %v", root, got, want)
		}
	}
	if _, read := read.Sources["off"]; read {
		t.Error("the read read a thread off the subtree")
	}
	want := map[string]ThreadRestSources{
		"root": {Unarmed: []string{}},
		"a": {AnswerDue: true, Wake: true, WakeKind: AgentWakeLoop, WakeLabel: "board", Reminder: true, ReminderNote: "check the board",
			OpenWork: true, Unarmed: []string{"quiet-send"}},
		"b": {Workflow: true, WorkflowGoal: "review it", Unarmed: []string{}},
		"c": {Workflow: true, WorkflowGoal: "ship it", OpenWork: true, Unarmed: []string{}},
	}
	for id, src := range want {
		if got := read.Sources[id]; fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", src) {
			t.Errorf("sources of %s = %+v; want %+v", id, got, src)
		}
	}
	one, err := s.ReadThreadRestSources("a", nil)
	if err != nil || fmt.Sprintf("%+v", one) != fmt.Sprintf("%+v", want["a"]) {
		t.Errorf("one thread's sources = %+v, %v; want %+v", one, err, want["a"])
	}
	if threads, err := s.ThreadsOfRestChanges([]string{"armed-send", "gone"}, []string{"run-c"}); err != nil {
		t.Fatal(err)
	} else if slices.Sort(threads); !slices.Equal(threads, []string{"a", "c", "peer"}) {
		t.Errorf("threads of the changes = %v; want the send's caller and thread and the run's", threads)
	}
}

// A subtree read costs its subtree: the virtual machine steps of its
// statements are the same with nothing else on the computer as with busy
// threads off the subtree, each with open work, a loop, a reminder, an
// answer on its way, a running workflow run and a remote spawn reporting
// open work, and as with a reached thread whose settled sends and asks
// number hundreds. One more thread below costs more.
func TestASubtreeReadCostsItsSubtree(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root", "child"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "to-root", "lead", ThreadRequestSpawn, "root", false, 10)
	settleAgentRequest(t, s, "to-root", 11)
	seedAgentRequest(t, s, "to-child", "root", ThreadRequestSpawn, "child", false, 12)
	settleAgentRequest(t, s, "to-child", 13)
	steps := func(threads ...string) int {
		t.Helper()
		walk := statementVMSteps(t, s, subtreeReachedSQL, `["root"]`)
		encoded := `["` + threads[0] + `"`
		for _, id := range threads[1:] {
			encoded += `,"` + id + `"`
		}
		return walk + statementVMSteps(t, s, threadRestSourcesSQL, encoded+`]`, `[]`)
	}
	quiet := steps("root", "child")

	for i := range 200 {
		id := fmt.Sprintf("busy-%03d", i)
		at := int64(1000 + 10*i)
		mustCreateThread(t, s, id)
		seedAgentRequest(t, s, "to-"+id, "lead", ThreadRequestSpawn, id, false, at)
		settleAgentRequest(t, s, "to-"+id, at+1)
		seedAgentRequest(t, s, "open-"+id, id, ThreadRequestSend, "lead", true, at+2)
		seedAgentRequest(t, s, "owed-"+id, id, ThreadRequestAsk, "lead", true, at+3)
		settleAgentRequestOwed(t, s, "owed-"+id, at+4)
		if _, err := s.RegisterAgentWake(AgentWake{ID: "w-" + id, ThreadID: id, Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 1 << 50}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertThreadRequest(ThreadRequest{Token: "remind-" + id, CallerThreadID: id, Kind: ThreadRequestRemind, Answer: []byte("later"),
			State: ThreadRequestAccepted, DueAt: 1 << 50, CreatedAt: at + 5, UpdatedAt: at + 5}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertThreadRequest(ThreadRequest{Token: "remote-" + id, CallerThreadID: id, Kind: ThreadRequestSpawn, TargetComputerID: "pc",
			TargetThreadID: "there-" + id, State: ThreadRequestFinished, NestedOpen: true, SettledAt: at + 7, CreatedAt: at + 6, UpdatedAt: at + 7}); err != nil {
			t.Fatal(err)
		}
		run := testWorkItem("run-"+id, "project-a", "running", at)
		run.OriginThreadID = id
		if err := s.CreateWorkItem(run); err != nil {
			t.Fatal(err)
		}
		// The reached child's history: settled sends and asks it made.
		for _, kind := range []string{ThreadRequestSend, ThreadRequestAsk} {
			token := fmt.Sprintf("hist-%s-%03d", kind, i)
			seedAgentRequest(t, s, token, "child", kind, "lead", true, at+8)
			settleAgentRequest(t, s, token, at+9)
		}
	}
	busy := steps("root", "child")
	if busy != quiet {
		t.Fatalf("the read takes %d steps with busy threads off the subtree and a long history below, %d without; want the same", busy, quiet)
	}

	mustCreateThread(t, s, "grandchild")
	seedAgentRequest(t, s, "to-grandchild", "child", ThreadRequestSpawn, "grandchild", false, 50_000)
	settleAgentRequest(t, s, "to-grandchild", 50_001)
	deeper := steps("root", "child", "grandchild")
	if deeper <= busy {
		t.Fatalf("one more thread below takes %d steps, the read before it %d; want more", deeper, busy)
	}
	t.Logf("a subtree read of two threads: %d steps, the same with 200 busy threads off it and 400 settled sends and asks below; a third thread: %d", quiet, deeper)
}
