package closer

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunParallelNoTasks(t *testing.T) {
	if errs := RunParallel(nil, time.Second); errs != nil {
		t.Fatalf("RunParallel(nil) = %v, want nil", errs)
	}
}

func TestRunParallelAllSucceed(t *testing.T) {
	var calls atomic.Int32
	tasks := []Task{
		{Label: "a", Close: func() error { calls.Add(1); return nil }},
		{Label: "b", Close: func() error { calls.Add(1); return nil }},
	}
	errs := RunParallel(tasks, time.Second)
	if errs != nil {
		t.Fatalf("RunParallel = %v, want nil", errs)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestRunParallelCollectsErrors(t *testing.T) {
	first := errors.New("first cause")
	second := errors.New("second cause")
	tasks := []Task{
		{Label: "ok", Close: func() error { return nil }},
		{Label: "alpha", Close: func() error { return first }},
		{Label: "beta", Close: func() error { return second }},
	}
	errs := RunParallel(tasks, time.Second)
	if len(errs) != 2 {
		t.Fatalf("len(errs) = %d, want 2; got %v", len(errs), errs)
	}
	gotMessages := make([]string, len(errs))
	for i, e := range errs {
		gotMessages[i] = e.Error()
	}
	joined := strings.Join(gotMessages, " ")
	if !strings.Contains(joined, "close alpha:") || !strings.Contains(joined, "close beta:") {
		t.Fatalf("messages = %v, want labels propagated", gotMessages)
	}
}

func TestRunParallelTimesOutAbandoningPending(t *testing.T) {
	never := make(chan struct{})
	tasks := []Task{
		{Label: "fast", Close: func() error { return nil }},
		{Label: "slow", Close: func() error {
			<-never
			return nil
		}},
	}
	errs := RunParallel(tasks, 50*time.Millisecond)
	close(never)
	if len(errs) != 1 {
		t.Fatalf("len(errs) = %d, want 1 (timeout for slow); got %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "slow") || !strings.Contains(errs[0].Error(), "did not finish") {
		t.Fatalf("timeout error = %q, want mention of slow + did not finish", errs[0].Error())
	}
}
