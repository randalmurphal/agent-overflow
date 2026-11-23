package app

import (
	"testing"
	"time"

	"agent-overflow/internal/harnessrpc"
)

// TestHarnessThreadWorkReadsWhatTheThreadWaitsOn: HarnessThreadsWorking
// answers from threadWaitingOn, the answer the thread tools read. A thread
// whose turn is open is named with it, an idle thread is left out, and the
// same thread is left out once its session stops and settles the turn.
func TestHarnessThreadWorkReadsWhatTheThreadWaitsOn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	harness := NewHarness(f.app, HarnessPaths{})
	idle := f.plainThread(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "start the sweep")
	receipt := f.receipt(t, ack.Token)
	target := receipt.TargetThreadID
	index, ok := threadRequestTurnIndex(target, receipt.TurnID)
	if !ok {
		t.Fatalf("receipt names no turn of its own thread: %+v", receipt)
	}
	waitUntil(t, 10*time.Second, func() bool {
		live, err := f.adapter().LiveState(t.Context(), target)
		return err == nil && live.ActiveTurn && f.app.triage.OpenTurnIndex(target) == index
	})

	working, err := harness.HarnessThreadsWorking([]string{idle.ID, target})
	if err != nil {
		t.Fatalf("HarnessThreadsWorking: %v", err)
	}
	if len(working) != 1 || working[0].ThreadID != target || !working[0].ActiveTurn {
		t.Fatalf("working = %+v, want only %s, with its turn open", working, target)
	}

	// The held mock never answers an interrupt; a stop settles the turn.
	if err := f.app.StopSession(target); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	var last []harnessrpc.HarnessThreadWorkResult
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last answer after the stop: %+v", last)
		}
	})
	waitUntil(t, 10*time.Second, func() bool {
		last, err = harness.HarnessThreadsWorking([]string{idle.ID, target})
		return err == nil && len(last) == 0
	})
}
