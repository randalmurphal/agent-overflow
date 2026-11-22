package testutil

import (
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

// WaitBudget is how long a test may wait for work the code under test does
// not bound itself (a transfer's jobs, a paired computer's answer, a
// pairing exchange): waitBudget, or less when go test's -timeout ends
// sooner, ending 10 s before it. One number for every such wait, so a step
// slowed by a crowded machine is not failed by a guess of the test's own,
// and a genuine hang still fails in minutes with the test's own message.
func WaitBudget(t *testing.T) time.Duration {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return waitBudget
	}
	return min(waitBudget, time.Until(deadline.Add(-waitReportMargin)))
}
