package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/workflow/def"
)

// An API caller waits for the runner starts its command produced, and that wait
// used to be unbounded. The CLI's own call is not: `internal/aocli`'s rpcTimeout
// is 30s, so a runner start that never reported turned a verb the engine had
// COMMITTED into `context deadline exceeded` at the operator's terminal — and
// the retry that followed met an FSM refusal for the state the first call had
// already produced. The reply is bounded here so the engine is always the side
// that answers first, and the answer is the verb's own outcome.
func TestReplyStopsWaitingOnAWedgedRunnerStart(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"basic": onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})},
		projectIDs: []string{"project"},
		// The real budget is 20s. Short enough here that the test spends no time
		// proving a timeout exists, long enough that the run's synchronous
		// admission is never what the deadline interrupts.
		replyBudget: 150 * time.Millisecond,
	})
	item := testItem("wedged", "project", "basic", 0)
	wedged := make(chan struct{})
	h.runner.startWait[item.ID] = wedged
	h.runner.startErrs[item.ID] = errors.Join(ErrSetupFailed, errors.New("provisioning never returned"))

	began := time.Now()
	if err := h.engine.StartItem(item); err != nil {
		close(wedged)
		t.Fatalf("a start whose runner never reported failed the caller: %v", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("reply waited %v on a runner start that never settled", waited)
	}
	// The transition the verb asked for is what its success means, and it is
	// persisted: the run is admitted and running, with its start still in flight.
	requireItemState(t, h.store, item.ID, StateRunning, "")

	// Abandoning the future does not abandon the START. The command loop still
	// owns it, so the outcome the caller stopped waiting for reaches the run.
	close(wedged)
	requireEventualItemState(t, h, item.ID, StateNeedsHuman, ReasonSetupFailed)
}

// The bound changes when a caller stops waiting, never what they are told about
// what did happen: a start that fails inside the budget still fails the verb,
// exactly as before.
func TestRunnerStartFailureInsideTheBudgetStillReachesTheCaller(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		workflows:   map[string]def.Workflow{"basic": onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})},
		projectIDs:  []string{"project"},
		replyBudget: 30 * time.Second,
	})
	item := testItem("failing", "project", "basic", 0)
	h.runner.startErrs[item.ID] = errors.Join(ErrSetupFailed, errors.New("provision failed"))

	err := h.engine.StartItem(item)
	if !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("StartItem error = %v, want the runner's own setup failure", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
}

// The budget is the CALLER's, so it is one deadline across every future the
// command produced — including the futures a settled response carries. Spending
// it again per level is how a bound becomes unbounded, and re-arming it per
// future is how N wedged units become N budgets.
//
// Expiry also invents nothing: what settled is reported, what did not is
// dropped, and no timeout error joins the result — the verb committed, so a
// start nobody waited for must not read as the verb failing.
//
// The nested future is what makes this more than a timing assertion: the
// deadline has to still be readable AFTER a deeper level has already seen it,
// which is why it is a closed channel and not a timer's single value. Written
// the other way, this exact shape hangs forever.
func TestReplyWaitAbandonsUnsettledStartsOnOneSharedDeadline(t *testing.T) {
	nested := &runnerStartFuture{done: make(chan response, 1)} // never settles
	settled := &runnerStartFuture{done: make(chan response, 1)}
	settled.done <- response{err: errors.New("first start failed"), starts: []*runnerStartFuture{nested}}
	unsettled := &runnerStartFuture{done: make(chan response, 1)} // never settles

	expired := make(chan struct{})
	timer := time.AfterFunc(20*time.Millisecond, func() { close(expired) })
	defer timer.Stop()

	began := time.Now()
	err := waitEngineResponseWithin(
		response{starts: []*runnerStartFuture{settled, unsettled}},
		expired,
	)
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("the wait ran %v; the nested future re-armed the deadline", waited)
	}
	if err == nil || !strings.Contains(err.Error(), "first start failed") {
		t.Fatalf("wait error = %v, want the start that did settle reported", err)
	}
	if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("wait error = %v, want no failure invented for a start that simply had not answered", err)
	}
}

// Expiry reports everything that ALREADY settled, whatever ORDER the futures
// happen to be in. A wave produces one future per element; blocking on a slow
// one and returning at its deadline used to drop every sibling behind it — so a
// verb whose second unit had already failed `ErrSetupFailed` answered success,
// and the operator learned about the failure from nothing at all.
func TestReplyExpiryStillReportsSiblingsThatAlreadyFailed(t *testing.T) {
	hung := &runnerStartFuture{done: make(chan response, 1)} // never settles
	failed := &runnerStartFuture{done: make(chan response, 1)}
	failed.done <- response{err: errors.Join(ErrSetupFailed, errors.New("the second unit never provisioned"))}
	// A settled future BEHIND the hung one, carrying its own settled child: the
	// drain has to be as complete as the ordinary path, not a shallow peek.
	nested := &runnerStartFuture{done: make(chan response, 1)}
	nested.done <- response{err: errors.New("a nested start failed too")}
	carrier := &runnerStartFuture{done: make(chan response, 1)}
	carrier.done <- response{starts: []*runnerStartFuture{nested}}

	expired := make(chan struct{})
	timer := time.AfterFunc(20*time.Millisecond, func() { close(expired) })
	defer timer.Stop()

	began := time.Now()
	err := waitEngineResponseWithin(
		response{starts: []*runnerStartFuture{hung, failed, carrier}},
		expired,
	)
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("the wait ran %v; the drain must not block on anything", waited)
	}
	if !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("wait error = %v, want the settled setup failure an unrelated hung sibling masked", err)
	}
	if err == nil || !strings.Contains(err.Error(), "a nested start failed too") {
		t.Fatalf("wait error = %v, want the settled nested failure reported too", err)
	}
}

// requireEventualItemState polls the run record for a transition the command
// loop takes after the caller has stopped waiting for it. It polls the STORE
// rather than syncing, because the whole point of the change under test is that
// the caller no longer has a handle on when that transition happens.
func requireEventualItemState(t *testing.T, h *testHarness, itemID string, state State, reason Reason) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		item, err := h.store.GetWorkItem(itemID)
		if err != nil {
			t.Fatal(err)
		}
		if State(item.State) == state && Reason(item.Reason) == reason {
			return
		}
		last = item.State + "/" + item.Reason
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("item %q rested at %s, want %s/%s", itemID, last, state, reason)
}
