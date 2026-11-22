package testutil

import (
	"testing"
	"time"
)

// waitReportMargin is how long before go test's timeout a wait gives up, so
// its test fails with its own message before the timeout's goroutine dump.
const waitReportMargin = 10 * time.Second

// WaitDeadline is when a test's poll for work the code under test does not
// bound itself (a transfer's jobs, a store write after a signal) gives up
// and reports what it saw: shortly before go test's -timeout. The wait gets
// the run's whole budget, so a step slowed by a loaded machine is not
// failed by a bound of the test's own, and a hang still ends in the test's
// message. With no timeout (-timeout 0) the poll waits as long as the run.
func WaitDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return time.Now().AddDate(100, 0, 0)
	}
	return deadline.Add(-waitReportMargin)
}
