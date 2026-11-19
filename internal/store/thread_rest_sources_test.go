package store

import "testing"

// A run owes a report for each move to a rest, counted in the write of
// that rest; a write that leaves the state or moves to running owes none.
// While it runs or owes one, a root run holds the thread it is bound to; a
// settled report ends its hold, a settle with none owed changes nothing,
// and the clear at the engine's start forgets every report owed.
func TestARunOwesAReportForEachRestUntilItIsSettled(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	item := testWorkItem("run", "project-a", "needs-human", 1)
	item.OriginThreadID = "worker"
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	held := func(want bool, after string) {
		t.Helper()
		src, err := s.ReadThreadRestSources("worker")
		if err != nil {
			t.Fatal(err)
		}
		threads, err := s.ThreadsHeldByWorkflowRuns()
		if err != nil {
			t.Fatal(err)
		}
		if src.Workflow != want || len(threads) == 1 != want {
			t.Fatalf("after %s the thread reads held %v and the held threads are %v; want held %v", after, src.Workflow, threads, want)
		}
	}
	move := func(state string) {
		t.Helper()
		if err := s.UpdateWorkItemState("run", state, "", 2); err != nil {
			t.Fatal(err)
		}
	}
	settle := func() {
		t.Helper()
		if err := s.SettleWorkItemReport("run"); err != nil {
			t.Fatal(err)
		}
	}
	held(false, "its creation parked")
	move("needs-human")
	held(false, "a write that left it parked")
	move("running")
	held(true, "running")
	move("running")
	move("done")
	held(true, "its rest")
	settle()
	held(false, "its report settled")
	settle()
	move("running")
	move("needs-human")
	move("done")
	settle()
	held(true, "one of its two reports settled")
	settle()
	held(false, "both settled")
	move("running")
	move("failed")
	move("running")
	move("cancelled")
	if err := s.ClearWorkItemReportsOwed(); err != nil {
		t.Fatal(err)
	}
	held(false, "the clear")
}
