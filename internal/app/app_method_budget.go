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
	budget := transport.MethodBudget(method)
	return context.WithTimeoutCause(ctx, budget, budgetExceeded{doing: doing, budget: budget})
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
