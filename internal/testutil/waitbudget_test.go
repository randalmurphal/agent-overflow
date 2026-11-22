package testutil

import (
	"testing"
	"time"
)

// A wait gets the shared budget, and never runs into the last 10 s before go
// test's own timeout.
func TestWaitBudgetIsTheSharedBudgetCutShortByTheRunsTimeout(t *testing.T) {
	timeout, ok := t.Deadline()
	if !ok {
		t.Skip("the run has no timeout")
	}
	// Read before the call, so the budget can only be shorter than this.
	untilLimit := time.Until(timeout.Add(-waitReportMargin))
	got := WaitBudget(t)
	if got > waitBudget {
		t.Fatalf("WaitBudget = %s, longer than the shared budget %s", got, waitBudget)
	}
	if got > untilLimit {
		t.Fatalf("WaitBudget = %s, past the %s left until %s before the run's timeout", got, untilLimit, waitReportMargin)
	}
	if time.Until(timeout) > waitBudget+waitReportMargin+time.Second && got < waitBudget-time.Second {
		t.Fatalf("WaitBudget = %s with %s left in the run, want the shared budget %s", got, time.Until(timeout), waitBudget)
	}
}
