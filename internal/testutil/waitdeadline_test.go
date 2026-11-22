package testutil

import "testing"

// A wait gives up shortly before go test's own timeout, and not sooner.
func TestWaitDeadlineIsJustBeforeTheRunsTimeout(t *testing.T) {
	timeout, ok := t.Deadline()
	if !ok {
		t.Skip("the run has no timeout")
	}
	got := WaitDeadline(t)
	if want := timeout.Add(-waitReportMargin); !got.Equal(want) {
		t.Fatalf("WaitDeadline = %s, want %s, %s before the run's timeout %s", got, want, waitReportMargin, timeout)
	}
}
