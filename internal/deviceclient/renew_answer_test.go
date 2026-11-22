package deviceclient

import (
	"context"
	"errors"
	"testing"
)

// finishedRenewal is a rotation that has already finished with err.
func finishedRenewal(err error) *renewal {
	r := &renewal{done: make(chan struct{}), err: err}
	close(r.done)
	return r
}

// endedAfterFirstCheck is a context that reports itself live to the check a
// waiter makes as it starts waiting and is ended by the time it waits, so
// the waiter finds the rotation and the cancellation both ready.
type endedAfterFirstCheck struct {
	context.Context
	checks int
}

func (c *endedAfterFirstCheck) Err() error {
	c.checks++
	if c.checks == 1 {
		return nil
	}
	return context.Canceled
}

func (c *endedAfterFirstCheck) Done() <-chan struct{} {
	ended := make(chan struct{})
	close(ended)
	return ended
}

// The rotation and the cancellation are both ready in each case below, so
// a select that chose between them would answer either. Each case runs
// many times so that such a choice cannot pass by luck; the answer itself
// does not depend on scheduling.
const answerRuns = 64

// A caller whose context had already ended when it started waiting hears
// its cancellation, even when the rotation has finished too.
func TestRenewalAnswersACallerAlreadyCancelledWithItsCancellation(t *testing.T) {
	flight := finishedRenewal(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range answerRuns {
		if err := flight.wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait with an already cancelled context = %v; want context.Canceled", err)
		}
	}
}

// A caller whose context ends while it waits hears the rotation's outcome
// when the rotation has finished by then.
func TestRenewalAnswersACallerCancelledDuringTheWaitWithAFinishedOutcome(t *testing.T) {
	outcome := errors.New("rotation outcome")
	flight := finishedRenewal(outcome)
	for range answerRuns {
		if err := flight.wait(&endedAfterFirstCheck{Context: context.Background()}); !errors.Is(err, outcome) {
			t.Fatalf("wait cancelled during the wait with the rotation finished = %v; want the rotation's outcome", err)
		}
	}
}
