package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunParallelClosersFinishesWithinTimeoutWhenAllSlow exercises Bug B4:
// ten closers each taking ~1.5 s must finish concurrently (total wall clock
// ~1.5 s), not sequentially (15 s). A regression that reverted to a
// serial loop would blow past the 5-second bound.
func TestRunParallelClosersFinishesWithinTimeoutWhenAllSlow(t *testing.T) {
	const (
		count   = 10
		delay   = 1500 * time.Millisecond
		timeout = 5 * time.Second
	)
	closers := make([]threadCloser, 0, count)
	var completed atomic.Int32
	for i := 0; i < count; i++ {
		label := fmt.Sprintf("slow-%d", i)
		closers = append(closers, threadCloser{
			label: label,
			close: func() error {
				time.Sleep(delay)
				completed.Add(1)
				return nil
			},
		})
	}

	start := time.Now()
	errs := runParallelClosers(closers, timeout)
	elapsed := time.Since(start)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if completed.Load() != count {
		t.Fatalf("completed = %d, want %d", completed.Load(), count)
	}
	// Parallel execution budget: delay + scheduling slack. Sequential
	// execution would take 15 s and fail this assertion.
	if elapsed > delay+2*time.Second {
		t.Fatalf("shutdown took %v, expected <%v (parallel execution regression)", elapsed, delay+2*time.Second)
	}
}

// TestRunParallelClosersTimesOutOnHangingCloser exercises the hard-cap
// behaviour: one hung closer must not block the rest of the teardown.
// The deadline returns within the window and the hanging closer is
// reported as a timeout error.
func TestRunParallelClosersTimesOutOnHangingCloser(t *testing.T) {
	const timeout = 500 * time.Millisecond

	hungRelease := make(chan struct{})
	defer close(hungRelease)

	closers := []threadCloser{
		{label: "fast-1", close: func() error { return nil }},
		{label: "fast-2", close: func() error {
			time.Sleep(100 * time.Millisecond)
			return nil
		}},
		{label: "hanger", close: func() error {
			// Blocks until the test releases it at the end. A
			// shutdown regression that Wait'd on this closer would
			// block test teardown.
			<-hungRelease
			return nil
		}},
	}

	start := time.Now()
	errs := runParallelClosers(closers, timeout)
	elapsed := time.Since(start)

	if elapsed > timeout+500*time.Millisecond {
		t.Fatalf("shutdown took %v, expected <%v (hanging closer blocked return)", elapsed, timeout+500*time.Millisecond)
	}

	// We expect exactly one timeout error, for the hanger. The two fast
	// closers must not have produced timeouts.
	var timeoutCount, hangerSeen int
	for _, err := range errs {
		if strings.Contains(err.Error(), "did not finish") {
			timeoutCount++
			if strings.Contains(err.Error(), "hanger") {
				hangerSeen++
			}
		}
	}
	if timeoutCount != 1 {
		t.Fatalf("timeout errors = %d, want 1 (errs=%v)", timeoutCount, errs)
	}
	if hangerSeen != 1 {
		t.Fatalf("hanger timeout not reported (errs=%v)", errs)
	}
}

// TestRunParallelClosersSurfacesIndividualErrors checks that a failure in
// one closer is surfaced without preventing the others from running.
func TestRunParallelClosersSurfacesIndividualErrors(t *testing.T) {
	closers := []threadCloser{
		{label: "ok-1", close: func() error { return nil }},
		{label: "bad-1", close: func() error {
			return fmt.Errorf("intentional failure")
		}},
		{label: "ok-2", close: func() error { return nil }},
	}

	errs := runParallelClosers(closers, 2*time.Second)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one", errs)
	}
	if !strings.Contains(errs[0].Error(), "intentional failure") {
		t.Fatalf("error message does not mention failure: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "bad-1") {
		t.Fatalf("error message does not mention closer label: %v", errs[0])
	}
}

// TestRunParallelClosersEmpty is a defensive check — no closers means no
// errors, no goroutine leaks, no deadline to hit.
func TestRunParallelClosersEmpty(t *testing.T) {
	errs := runParallelClosers(nil, time.Second)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}
