package testutil

import "context"

// DeadlineContext is a context whose deadline the test ends by calling end:
// from then on it is done with context.DeadlineExceeded and the given
// cause, as a context past its deadline is. The test chooses the moment
// (a request held at a fake forge, provably in flight or provably not yet
// started), so no real duration races the code under test.
func DeadlineContext(parent context.Context) (ctx context.Context, end func(cause error)) {
	inner, cancel := context.WithCancelCause(parent)
	return deadlineContext{inner}, cancel
}

type deadlineContext struct{ context.Context }

func (c deadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}
