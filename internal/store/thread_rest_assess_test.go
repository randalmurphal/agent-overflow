package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

// everySpawnRests guards a sweep at cutoff with every thread of a spawn
// past it read at rest: the sweep on a computer whose threads all rest.
func everySpawnRests(t *testing.T, s *Store, cutoff int64) *RestGuard {
	t.Helper()
	roots, err := s.SpawnThreadsPastFloor(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	return &RestGuard{ReconciledComputers: fixtureRemoteComputers(t, s), Roots: roots, AtRest: func(subtree SubtreeRest) []string {
		return slices.Sorted(maps.Keys(subtree.Reached))
	}}
}

// storeSourcesRest guards a sweep at cutoff with the store's sources: a
// root rests when every thread it reaches reads at rest by them, what the
// app answers when nothing in memory keeps those threads busy.
func storeSourcesRest(t *testing.T, s *Store, cutoff int64) *RestGuard {
	t.Helper()
	roots, err := s.SpawnThreadsPastFloor(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	return &RestGuard{ReconciledComputers: fixtureRemoteComputers(t, s), Roots: roots, AtRest: durableSourcesRest}
}

func fixtureRemoteComputers(t *testing.T, s *Store) []string {
	t.Helper()
	ids, err := s.queryThreadIDs("fixture computers", `SELECT DISTINCT target_computer_id FROM thread_requests WHERE target_computer_id <> ''`)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// durableSourcesRest names the roots of subtree every thread of which reads
// at rest by its durable sources.
func durableSourcesRest(subtree SubtreeRest) []string {
	var out []string
	for root, reached := range subtree.Reached {
		rests := true
		for _, id := range reached {
			src := subtree.Sources[id]
			rests = rests && !src.Asking && !src.AnswerDue && !src.Background && !src.Workflow && !src.Wake && !src.Reminder && !src.OpenWork
		}
		if rests {
			out = append(out, root)
		}
	}
	slices.Sort(out)
	return out
}

// guardOf guards roots with atRest.
func guardOf(atRest func(SubtreeRest) []string, roots ...string) *RestGuard {
	return &RestGuard{Roots: roots, AtRest: atRest}
}

func armLoop(t *testing.T, s *Store, id, threadID string) {
	t.Helper()
	if _, err := s.RegisterAgentWake(AgentWake{ID: id, ThreadID: threadID, Kind: AgentWakeLoop, Label: "board", IntervalSeconds: 60, NextAt: 1 << 50}); err != nil {
		t.Fatal(err)
	}
}

// A spawn past the floor whose thread is here is cleared only when its
// thread reads at rest inside the sweep's transaction: a sweep with no
// guard, or one whose read there leaves the thread out, leaves it whole on
// both sides, and one whose read names it keeps only its lineage.
func TestTheSweepClearsNoWorkingSpawnUnlessItsWriteReadsItAtRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root", "other"} {
		mustCreateThread(t, s, id)
	}
	spawnWithReceipt(t, s, "to-root", "lead", "root", 200)
	spawnWithReceipt(t, s, "to-other", "lead", "other", 200)
	if threads, err := s.SpawnThreadsPastFloor(1000); err != nil || !slices.Equal(threads, []string{"other", "root"}) {
		t.Fatalf("threads of the spawns past the floor = %v, %v; want both", threads, err)
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
	every := func(subtree SubtreeRest) []string { return slices.Sorted(maps.Keys(subtree.Reached)) }
	if _, _, err := s.DeleteThreadRequestsBefore(1000, nil); err != nil {
		t.Fatal(err)
	}
	whole("a sweep with no guard")
	if _, _, err := s.DeleteThreadRequestsBefore(1000, guardOf(every, "other")); err != nil {
		t.Fatal(err)
	}
	whole("a sweep guarding another spawn's thread")
	if row := rawThreadRequest(t, s, "to-other"); !row.LineageOnly {
		t.Fatalf("the spawn whose thread the sweep read at rest: %+v; want its lineage", row)
	}
	if _, _, err := s.DeleteThreadRequestsBefore(1000, guardOf(func(SubtreeRest) []string { return nil }, "root")); err != nil {
		t.Fatal(err)
	}
	whole("a sweep whose write read the thread not at rest")
	if _, _, err := s.DeleteThreadRequestsBefore(1000, guardOf(every, "root")); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "to-root"); !row.LineageOnly || row.Answer != nil {
		t.Fatalf("the spawn once its thread read at rest: %+v; want its lineage alone", row)
	}
	if _, found, err := s.GetThreadRequestReceipt("to-root"); err != nil || found {
		t.Fatalf("the receipt once its thread read at rest: found %v, %v; want its lineage alone", found, err)
	}
}

// The guard reads inside the write's transaction what was committed before
// it: a loop armed on a thread after its rest was decided keeps the spawn
// whole, for the sweep and for a settlement.
func TestTheGuardReadsWhatWasCommittedBeforeTheWrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "root", "child", "worker"} {
		mustCreateThread(t, s, id)
	}
	spawnWithReceipt(t, s, "to-root", "lead", "root", 200)
	seedAgentRequest(t, s, "to-child", "root", ThreadRequestSpawn, "child", false, 210)
	settleAgentRequest(t, s, "to-child", 211)
	sweep := storeSourcesRest(t, s, 1000)
	armLoop(t, s, "child-loop", "child")
	if _, _, err := s.DeleteThreadRequestsBefore(1000, sweep); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "to-root"); row.LineageOnly {
		t.Fatalf("the spawn whose subtree armed a loop after its rest was read: %+v; want it whole", row)
	}

	acceptReceipt(t, s, "to-worker", "lead", "worker", 300)
	settle := guardOf(durableSourcesRest, "worker")
	armLoop(t, s, "worker-loop", "worker")
	finished := ThreadRequestSettlement{State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 400}
	result, err := s.SettleLocalThreadRequestAtRest("to-worker", ThreadReceiptOpenStates(), finished, ThreadRequestFinished, settle)
	if err != nil || result.Settled || !result.Unrested {
		t.Fatalf("a settlement whose thread armed a loop after its rest was read = %+v, %v; want unrested, nothing written", result, err)
	}
	if receipt, _, err := s.GetThreadRequestReceipt("to-worker"); err != nil || receipt.State == ThreadReceiptFinished {
		t.Fatalf("the receipt = %+v, %v; want it open", receipt, err)
	}
	if result, err := s.SettleLocalThreadRequestAtRest("to-worker", ThreadReceiptOpenStates(), finished, ThreadRequestFinished, guardOf(durableSourcesRest)); err != nil || !result.Unrested {
		t.Fatalf("a settlement whose guard names no root at rest = %+v, %v; want unrested", result, err)
	}
	every := func(subtree SubtreeRest) []string { return slices.Sorted(maps.Keys(subtree.Reached)) }
	if result, err := s.SettleLocalThreadRequestAtRest("to-worker", ThreadReceiptOpenStates(), finished, ThreadRequestFinished, guardOf(every, "worker")); err != nil || !result.Settled {
		t.Fatalf("a settlement whose thread reads at rest = %+v, %v; want it settled", result, err)
	}
}

// acceptReceipt opens a local spawn and its receipt, not yet settled.
func acceptReceipt(t *testing.T, s *Store, token, caller, target string, at int64) {
	t.Helper()
	seedAgentRequest(t, s, token, caller, ThreadRequestSpawn, target, false, at)
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "local", Kind: ThreadRequestSpawn,
		SourceThreadID: caller, TargetThreadID: target, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
}

// On a store with one connection, where every read and the writer share
// it, the guard reads on the write's own transaction: the sweep and a
// settlement both complete.
func TestTheGuardReadsOnTheWritesConnection(t *testing.T) {
	t.Parallel()
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			// A write stuck on the one connection would hold Close too.
			return
		}
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if s.reader() != s.db {
		t.Fatal("the store reads on a pool; want one connection")
	}
	if _, err := s.CreateProject(Project{ID: defaultTestProjectID, Path: "/tmp/test", Name: "One", Slug: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"lead", "root", "worker"} {
		mustCreateThread(t, s, id)
	}
	spawnWithReceipt(t, s, "to-root", "lead", "root", 200)
	acceptReceipt(t, s, "to-worker", "lead", "worker", 300)
	within := func(what string, write func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- write() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s waits for its own connection", what)
		}
	}
	within("the sweep", func() error {
		_, _, err := s.DeleteThreadRequestsBefore(1000, storeSourcesRest(t, s, 1000))
		return err
	})
	if row := rawThreadRequest(t, s, "to-root"); !row.LineageOnly {
		t.Fatalf("the spawn after the sweep: %+v; want its lineage", row)
	}
	within("the settlement", func() error {
		result, err := s.SettleLocalThreadRequestAtRest("to-worker", ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 400,
		}, ThreadRequestFinished, guardOf(durableSourcesRest, "worker"))
		if err == nil && !result.Settled {
			err = fmt.Errorf("not settled: %+v", result)
		}
		return err
	})
}

// insertRemoteWatch records a command thread runs on computer, running
// unless receipt says otherwise.
func insertRemoteWatch(t *testing.T, s *Store, request, threadID, computer, label, receipt, notification string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO remote_watches(computer_id,request_id,thread_id,launched_in_thread_id,fingerprint,label,receipt,notification,created_at)
		VALUES(?,?,?,?,'',?,?,?,1)`, computer, request, threadID, threadID, label, receipt, notification); err != nil {
		t.Fatal(err)
	}
}

// insertBackgroundLaunch records a live background launch in threadID.
func insertBackgroundLaunch(t *testing.T, s *Store, id, threadID, summary string) {
	t.Helper()
	if err := s.InsertItem(Item{ID: id, ThreadID: threadID, TurnIndex: 1, ItemIndex: 1, Role: "assistant", Kind: "tool_call", Status: "running",
		Summary: summary, ToolName: "Bash", IsBackground: true, CreatedAt: 1000}); err != nil {
		t.Fatal(err)
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
	armLoop(t, s, "w", "a")
	if err := s.InsertThreadRequest(ThreadRequest{Token: "remind", CallerThreadID: "a", Kind: ThreadRequestRemind, Answer: []byte("check the board"),
		State: ThreadRequestAccepted, DueAt: 1 << 50, CreatedAt: 104, UpdatedAt: 104}); err != nil {
		t.Fatal(err)
	}
	// c's: a workflow run bound to it, running, and a spawn on another
	// computer that reports open work. b's: a run that ended, which holds
	// it only while the workflow service owes its report, and a command on
	// another computer. root's: a live background launch.
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
	insertRemoteWatch(t, s, "deploy", "b", "pc", "deploy", `{}`, "pending")
	insertRemoteWatch(t, s, "ended", "c", "pc", "ended", `{"id":"j","state":"finished"}`, "pending")
	insertRemoteWatch(t, s, "dismissed", "c", "pc", "dismissed", `{}`, "dismissed")
	insertBackgroundLaunch(t, s, "launch", "root", "npm test")
	// c's latest turn is open; b's open turn is older than its latest,
	// which ended.
	for _, turn := range []struct {
		thread string
		index  int
		ended  bool
	}{{"c", 1, true}, {"c", 2, false}, {"b", 1, false}, {"b", 2, true}} {
		id := fmt.Sprintf("%s:%d", turn.thread, turn.index)
		if err := s.InsertTurn(Turn{TurnID: id, ThreadID: turn.thread, TurnIndex: turn.index, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if turn.ended {
			if err := s.UpdateTurnCompleted(id, 2, "end_turn", "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	read, err := s.ReadSubtreeRest([]string{"root", "b"})
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
		"root": {Background: true, BackgroundDetail: "npm test", Unarmed: []string{}},
		"a": {AnswerDue: true, Wake: true, WakeKind: AgentWakeLoop, WakeLabel: "board", Reminder: true, ReminderNote: "check the board",
			OpenWork: true, Unarmed: []string{"quiet-send"}},
		"b": {Background: true, BackgroundDetail: "deploy", BackgroundComputer: "pc", Unarmed: []string{}},
		"c": {TurnOpen: true, Workflow: true, WorkflowGoal: "ship it", OpenWork: true, Unarmed: []string{}},
	}
	for id, src := range want {
		if got := read.Sources[id]; fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", src) {
			t.Errorf("sources of %s = %+v; want %+v", id, got, src)
		}
	}
	one, err := s.ReadThreadRestSources("a")
	if err != nil || fmt.Sprintf("%+v", one) != fmt.Sprintf("%+v", want["a"]) {
		t.Errorf("one thread's sources = %+v, %v; want %+v", one, err, want["a"])
	}
}

// A subtree read costs its subtree: the virtual machine steps of its
// statements are the same with nothing else on the computer as with busy
// threads off the subtree, each with an open turn, open work, a loop, a
// reminder, an answer on its way, a running workflow run, 500 ended runs,
// live background work, a command on another computer and a remote spawn
// reporting open work, and as with a reached thread whose ended turns and
// settled sends and asks number hundreds. One more thread below costs more.
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
		encoded, err := json.Marshal(threads)
		if err != nil {
			t.Fatal(err)
		}
		return statementVMSteps(t, s, subtreeReachedSQL, `["root"]`) + statementVMSteps(t, s, threadRestSourcesSQL, string(encoded))
	}
	// endedTurn gives the reached child a turn that ended.
	endedTurn := func(index int, at int64) {
		t.Helper()
		turn := fmt.Sprintf("child:%d", index)
		if err := s.InsertTurn(Turn{TurnID: turn, ThreadID: "child", TurnIndex: index, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateTurnCompleted(turn, at+1, "end_turn", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	endedTurn(0, 1)
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
		armLoop(t, s, "w-"+id, id)
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
		insertBackgroundLaunch(t, s, "launch-"+id, id, "npm test")
		insertRemoteWatch(t, s, "deploy-"+id, id, "pc", "deploy", `{}`, "pending")
		if err := s.InsertTurn(Turn{TurnID: id + ":1", ThreadID: id, TurnIndex: 1, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
		// The reached child's history: turns that ended, and settled sends
		// and asks it made.
		endedTurn(i+1, at)
		for _, kind := range []string{ThreadRequestSend, ThreadRequestAsk} {
			token := fmt.Sprintf("hist-%s-%03d", kind, i)
			seedAgentRequest(t, s, token, "child", kind, "lead", true, at+8)
			settleAgentRequest(t, s, token, at+9)
		}
	}
	for i := range 500 {
		ended := testWorkItem(fmt.Sprintf("ended-%03d", i), "project-a", "done", int64(5000+i))
		ended.OriginThreadID = fmt.Sprintf("busy-%03d", i%200)
		if err := s.CreateWorkItem(ended); err != nil {
			t.Fatal(err)
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
	t.Logf("a subtree read of two threads: %d steps, the same with 200 busy threads and 500 ended runs off it and 200 ended turns and 400 settled sends and asks below; a third thread: %d", quiet, deeper)
}

// The runs a reached thread has finished cost its read nothing: the
// sources' statement takes as many virtual machine steps with 500 runs
// bound to the thread that ran, rested and had their reports delivered as
// with none, and the thread reads at rest.
func TestAReachedThreadsFinishedRunsCostItsReadNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	quiet := statementVMSteps(t, s, threadRestSourcesSQL, `["worker"]`)
	for i := range 500 {
		item := testWorkItem(fmt.Sprintf("old-%03d", i), "project-a", "running", int64(100+i))
		item.OriginThreadID = "worker"
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateWorkItemState(item.ID, "done", "", int64(200+i)); err != nil {
			t.Fatal(err)
		}
		if err := s.SettleWorkItemReport(item.ID); err != nil {
			t.Fatal(err)
		}
	}
	history := statementVMSteps(t, s, threadRestSourcesSQL, `["worker"]`)
	read, err := s.ReadSubtreeRest([]string{"worker"})
	if err != nil || read.Sources["worker"].Workflow {
		t.Fatalf("a thread whose runs all ended: %+v, %v; want it at rest", read, err)
	}
	if history != quiet {
		t.Fatalf("the read takes %d steps with 500 ended runs bound to the thread, %d with none; want the same", history, quiet)
	}
}
