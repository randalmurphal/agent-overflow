package engine

import (
	"errors"
	"time"
)

// The bound on how long ONE API call's reply waits for the runner starts its
// command produced. The command loop itself is never involved: it has already
// committed and answered, and everything here runs on the caller's goroutine.

// runnerStartFuture is one in-flight `Runner.Start` a command produced, as seen
// by the caller waiting on the command's reply. The command loop owns the other
// end: it removes the future from `inflightStarts` and applies its outcome to
// FSM state whether or not a caller is still listening.
type runnerStartFuture struct {
	key  RunKey
	done chan response
}

// settleRunnerStart delivers a start's result to whoever is waiting, or to
// nobody. The send is NON-BLOCKING and `done` is buffered with room for the one
// result a future can carry, which is what makes abandoning a future safe: a
// worker whose caller has already given up still settles and still exits.
func settleRunnerStart(start *runnerStartFuture, result response) {
	select {
	case start.done <- result:
	default:
	}
}

// runnerStartReplyBudget bounds the TOTAL time one API call's reply waits on the
// runner-start futures its command produced — one deadline across all of them,
// not one each, because a caller experiences the sum.
//
// It exists because the two sides of the RPC disagreed about what "no answer
// yet" means. `internal/aocli`'s rpcTimeout is 30s (`session.go`), so a reply
// held longer than that reaches the operator as `context deadline exceeded` for
// a verb that had already COMMITTED server-side — and their retry then hits an
// FSM refusal for the state their first call successfully produced. 20s keeps
// the engine the side that answers first, with room for the client's own hop.
// The two constants are a pair: neither moves without the other, and this one
// stays comfortably under it.
//
// Expiry is not an error. The command-loop transition is what the verb asked
// for and it has already been persisted; the runner start is the run's own next
// step, and its outcome reaches the operator the way every other asynchronous
// outcome does — `workflow:phase-state`, a park, and the engine log
// (`LogEventRunnerStart` or the park's cause). Reporting a timeout here would
// claim the verb failed when it did not.
const runnerStartReplyBudget = 20 * time.Second

// waitEngineResponse joins the command's own error with every runner-start
// failure that settles inside the reply budget.
//
// Abandoning a future is safe by construction and must stay that way; see
// `settleRunnerStart`. The reply is a listener, never the owner.
func (e *Engine) waitEngineResponse(result response) error {
	if len(result.starts) == 0 {
		return result.err
	}
	// The deadline is a CLOSED channel rather than a timer's one value, because
	// every level of the wait selects on it: a `<-timer.C` is delivered to the
	// first select that reaches it and leaves every later one waiting forever,
	// which is a bound that turns into a hang the moment a command produces two
	// futures.
	expired := make(chan struct{})
	timer := time.AfterFunc(e.startReplyBudget, func() { close(expired) })
	defer timer.Stop()
	return waitEngineResponseWithin(result, expired)
}

// waitEngineResponseWithin collects settled results until the shared deadline
// fires. One expiry ends the whole wait — including the nested futures a settled
// response may itself carry — because the budget is the CALLER's, and spending
// it again per level is how a bound becomes unbounded.
//
// Expiry still reports everything that ALREADY settled. The futures are
// independent — a command that starts a wave produces one per element — so
// blocking on the first slow one and returning at its deadline would MASK a
// sibling that had already failed with `ErrSetupFailed`, answering success for a
// verb a start had already proved otherwise. The drain is non-blocking, so it
// spends none of a budget that is by then gone; nothing is invented for what did
// not settle.
func waitEngineResponseWithin(result response, expired <-chan struct{}) error {
	errs := []error{result.err}
	for index, start := range result.starts {
		select {
		case settled := <-start.done:
			errs = append(errs, waitEngineResponseWithin(settled, expired))
		case <-expired:
			for _, remaining := range result.starts[index:] {
				select {
				case settled := <-remaining.done:
					errs = append(errs, waitEngineResponseWithin(settled, expired))
				default:
				}
			}
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}
