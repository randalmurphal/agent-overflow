package serialqueue

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestJobsRunInSubmissionOrderOneAtATime(t *testing.T) {
	var q Queue
	var mu sync.Mutex
	var order []int
	var running atomic.Int32

	for i := range 50 {
		q.Go(func() {
			if running.Add(1) != 1 {
				t.Errorf("two jobs running concurrently")
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			running.Add(-1)
		})
	}
	q.Wait()

	if len(order) != 50 {
		t.Fatalf("ran %d jobs, want 50", len(order))
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("order[%d] = %d, want %d (submission order)", i, got, i)
		}
	}
}

// TestWaitCoversJobsSubmittedFromAJob pins the doc's claim that Wait only
// reports done with an empty queue: a job that enqueues follow-up work (the
// workflow-reaction shape — a transition's handler schedules the next) is
// drained by the same Wait.
func TestWaitCoversJobsSubmittedFromAJob(t *testing.T) {
	var q Queue
	var ran atomic.Int32
	q.Go(func() {
		ran.Add(1)
		q.Go(func() { ran.Add(1) })
	})
	q.Wait()
	if got := ran.Load(); got != 2 {
		t.Fatalf("Wait returned with %d of 2 jobs run", got)
	}
}

func TestQueueRestartsAfterDraining(t *testing.T) {
	var q Queue
	var ran atomic.Int32
	q.Go(func() { ran.Add(1) })
	q.Wait()
	q.Go(func() { ran.Add(1) })
	q.Wait()
	if got := ran.Load(); got != 2 {
		t.Fatalf("second submission after a drain ran %d of 2 jobs", got)
	}
}
