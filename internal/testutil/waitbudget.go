package testutil

import (
	"fmt"
	"testing"
	"time"
)

// waitBudget is how long a test waits for work it does not bound itself.
// Generous against a crowded machine: the slowest such step measured under
// load from other test runs took about 25 s (a thread transfer), and a
// pairing about 11 s.
const waitBudget = 2 * time.Minute

// waitReportMargin is how long before go test's timeout a wait gives up, so
// its test fails with its own message before the timeout's goroutine dump.
const waitReportMargin = 10 * time.Second

// WaitBudget is how long a test may wait, from now, for work the code under
// test does not bound itself (a transfer's jobs, a paired computer's
// answer): waitBudget, or less when go test's -timeout ends sooner, ending
// 10 s before it. One number for every such wait, so a step slowed by a
// crowded machine is not failed by a guess of the test's own, and a genuine
// hang still fails in minutes with the test's own message. When no budget is
// left it fails the test at once, so call it from the test's goroutine.
func WaitBudget(t *testing.T) time.Duration {
	t.Helper()
	budget, err := StepBudget(t)()
	if err != nil {
		t.Fatal(err)
	}
	return budget
}

// StepBudget is WaitBudget asked when a step starts, from any goroutine: the
// bound a test gives a step of the code under test that would otherwise use
// its product bound (attachedbackends.Manager.SetPairingBound,
// deviceclient.WithRedemptionBound). Once no budget is left it answers an
// error, and the step fails at once with it.
func StepBudget(t *testing.T) func() (time.Duration, error) {
	deadline, ok := t.Deadline()
	return func() (time.Duration, error) { return budgetLeft(deadline, ok, time.Now()) }
}

// budgetLeft is the budget at now for a run whose go test timeout is
// deadline (ok false: the run has none).
func budgetLeft(deadline time.Time, ok bool, now time.Time) (time.Duration, error) {
	if !ok {
		return waitBudget, nil
	}
	left := deadline.Add(-waitReportMargin).Sub(now)
	if left <= 0 {
		return 0, fmt.Errorf("testutil: no wait budget is left: the run's timeout is %s away, inside the last %s that are kept for the test to report", deadline.Sub(now).Round(time.Millisecond), waitReportMargin)
	}
	return min(waitBudget, left), nil
}
