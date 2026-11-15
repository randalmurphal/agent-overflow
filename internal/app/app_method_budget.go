package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/transport"
)

// methodBudgetExceededCode is the wire code of a method run cut at its
// declared budget.
const methodBudgetExceededCode = "method_budget_exceeded"

// budgetExceeded is the cause of a run cut at its method's budget.
type budgetExceeded struct {
	doing  string
	budget time.Duration
}

func (e budgetExceeded) Error() string {
	return fmt.Sprintf("%s took longer than %s, so it was stopped", e.doing, e.budget)
}

// withinMethodBudget bounds the whole run of a method that repeats a
// bounded step (a download per artifact, a resumed log fetch, a chunk per
// call) by the method's declared //ao:budget, so the method's server bound
// is its budget by construction and its client, which waits a margin past
// it, hears how the run ended. doing names the run for that answer.
func withinMethodBudget(ctx context.Context, method, doing string) (context.Context, context.CancelFunc) {
	return withinMethodBudgetFinishing(ctx, method, doing, 0)
}

// withinMethodBudgetFinishing is withinMethodBudget for a run whose step
// under way when its context ends runs to its end (a forge write that may
// have applied): the context ends finish before the budget, the longest
// that step and what follows it take, so the run still answers within
// its budget.
func withinMethodBudgetFinishing(ctx context.Context, method, doing string, finish time.Duration) (context.Context, context.CancelFunc) {
	cutoff := transport.MethodBudget(method) - finish
	return context.WithTimeoutCause(ctx, cutoff, budgetExceeded{doing: doing, budget: cutoff})
}

// budgetError is err, or the budget's own readable error once ctx ended at
// it.
func budgetError(ctx context.Context, err error) error {
	var exceeded budgetExceeded
	if err != nil && errors.As(context.Cause(ctx), &exceeded) {
		return errorsx.Public(methodBudgetExceededCode, exceeded.Error()+".", err)
	}
	return err
}
