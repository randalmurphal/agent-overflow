package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A request's work may start as the last one ends while shutdown, the
// harness or a test waits on threadRequestsWG: the emit path starts a
// drain from whatever goroutine emitted. The Wait returns at the moment
// the count reached zero, and the new member is counted for the next one.
// With one P the waiter cannot run between the Done and the Add, which is
// the order a sync.WaitGroup panics on ("WaitGroup is reused before
// previous Wait has returned"). Serial: it sets GOMAXPROCS.
func TestThreadRequestWorkMayStartAsAWaitReturns(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var a App
	g := &a.threadRequestsWG

	g.addMember(1)
	waited := make(chan struct{})
	go waitThreadRequestWork(g, waited)
	awaitParkedIn(t, "waitThreadRequestWork")
	select {
	case <-waited:
		t.Fatal("Wait returned with a member running")
	default:
	}

	g.addMember(-1)
	g.addMember(1)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return at the moment the count reached zero")
	}

	again := make(chan struct{})
	go waitThreadRequestWork(g, again)
	awaitParkedIn(t, "waitThreadRequestWork")
	g.addMember(-1)
	select {
	case <-again:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return once the member that started during the last one ended")
	}
	g.Wait()
}

func waitThreadRequestWork(g interface{ Wait() }, done chan<- struct{}) {
	g.Wait()
	close(done)
}

// awaitParkedIn yields until a goroutine running fn is blocked in a Wait.
func awaitParkedIn(t *testing.T, fn string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			// The package's other tests leave goroutines behind; the
			// waiter may be past the end of a short dump.
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(stack, "."+fn+"(") && strings.Contains(header, ".Wait") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine in %s parked in a Wait", fn)
}

// Close waits for the members running and refuses every later one.
func TestWorkGroupCloseJoinsAndRefuses(t *testing.T) {
	t.Parallel()
	var g workGroup
	release := make(chan struct{})
	if !g.Go(func() { <-release }) {
		t.Fatal("Go refused before Close")
	}
	closed := make(chan struct{})
	go func() {
		g.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned with a member running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return once the member ended")
	}
	ran := false
	if g.Go(func() { ran = true }) || ran {
		t.Fatal("Go ran a member after Close")
	}
	g.Close()
}

// Every member of a workGroup starts through Go, which Close refuses: a
// count moved anywhere else would let work start after shutdown joined
// the group.
func TestWorkGroupMembersStartOnlyThroughGo(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "app_work_group.go" {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "addMember(") {
			t.Errorf("%s moves a workGroup count outside Go", file)
		}
	}
}

// Once threadRequestsWG is closed, each path that starts request work
// starts nothing and undoes what it claimed for the start. Only the
// group's refusal stops them here: the app's lifetime is not cancelled.
func TestRequestWorkAfterTheGroupClosesStartsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// start attempts the work and reports what the refusal left wrong.
		start func(a *App) string
	}{
		{"sweeps", func(a *App) string {
			a.startThreadRequestSweeps()
			return ""
		}},
		{"poll", func(a *App) string {
			if !a.beginThreadRequestPoll() {
				return "the poll slot was taken"
			}
			a.startThreadRequestPoll([]store.ThreadRequest{{Token: "t"}})
			if !a.beginThreadRequestPoll() {
				return "the refused poll kept the poll slot"
			}
			return ""
		}},
		{"wake run", func(a *App) string {
			ran := false
			if a.startAgentWakeRun(store.AgentWake{ID: "w", ThreadID: "th"}, func(context.Context, *agentWakeRun) { ran = true }) {
				return "startAgentWakeRun reported a start"
			}
			if _, registered := a.agentWakes.runs["w"]; registered || ran {
				return "the refused run stayed registered or ran"
			}
			return ""
		}},
		{"peer rows", func(a *App) string {
			c := &a.threadAgentPeers
			c.mu.Lock()
			defer c.mu.Unlock()
			c.queued, c.running, c.perComputer = map[string]bool{"s": true}, map[string]bool{}, map[string]int{}
			c.waiting = []store.ThreadRequest{{Token: "s", TargetComputerID: "c"}}
			a.startPeerAgentRowsLocked()
			if c.inFlight != 0 || len(c.running) != 0 || c.perComputer["c"] != 0 {
				return fmt.Sprintf("the refused call is counted: inFlight %d running %v", c.inFlight, c.running)
			}
			return ""
		}},
		{"agent row emits", func(a *App) string {
			a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.callers["caller"] = struct{}{} })
			e := &a.threadAgentEmits
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.draining {
				return "the refused drain left the emits marked draining"
			}
			return ""
		}},
		{"settlement", func(a *App) string {
			ran := false
			if a.goThreadRequestSettling(func() { ran = true }) || ran {
				return "the settlement started"
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &App{}
			a.appCtx, a.appCancel = context.WithCancel(context.Background())
			t.Cleanup(a.appCancel)
			a.threadRequestsWG.Close()
			if problem := tc.start(a); problem != "" {
				t.Fatal(problem)
			}
			if n := a.threadRequestsWG.members(); n != 0 {
				t.Fatalf("%d members started after Close", n)
			}
		})
	}
}

// Between shutdown's cancel and its Close of threadRequestsWG, a
// settlement is refused by the app's lifetime alone.
func TestASettlementAfterTheAppIsCancelledIsRefused(t *testing.T) {
	t.Parallel()
	a := &App{}
	a.appCtx, a.appCancel = context.WithCancel(context.Background())
	a.appCancel()
	ran := false
	if a.goThreadRequestSettling(func() { ran = true }) {
		t.Fatal("goThreadRequestSettling started work after the app was cancelled")
	}
	a.threadRequestsWG.Wait()
	if ran || a.threadRequestsWG.members() != 0 {
		t.Fatal("work ran after the app was cancelled")
	}
}

// A wake whose delivery slot is free but whose settlement is refused gives
// the slot back and is dropped. The app is not cancelled, so the slot is
// the select's only ready case and the refusal is the group's.
func TestARefusedWakeGivesBackItsSlotAndIsDropped(t *testing.T) {
	t.Parallel()
	a := &App{}
	a.appCtx, a.appCancel = context.WithCancel(context.Background())
	t.Cleanup(a.appCancel)
	a.threadRequestsWG.Close()
	slots := a.threadWakeDeliverySlots()
	dropped, delivered := false, false
	a.runThreadWakeDetached(func() { delivered = true }, func() { dropped = true })
	if delivered || !dropped {
		t.Fatalf("delivered %v dropped %v; want the wake dropped", delivered, dropped)
	}
	if len(slots) != 0 {
		t.Fatalf("%d delivery slots held after the refusal", len(slots))
	}

	// Cancelled first, it is dropped before it takes a slot.
	cancelled := &App{}
	cancelled.appCtx, cancelled.appCancel = context.WithCancel(context.Background())
	cancelled.appCancel()
	dropped = false
	cancelled.runThreadWakeDetached(func() { delivered = true }, func() { dropped = true })
	if delivered || !dropped || len(cancelled.threadWakeDeliverySlots()) != 0 {
		t.Fatalf("cancelled: delivered %v dropped %v slots %d", delivered, dropped, len(cancelled.threadWakeDeliverySlots()))
	}
}
