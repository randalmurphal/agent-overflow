package closer

import (
	"fmt"
	"time"

	"agent-overflow/internal/errorsx"
)

// Task is a single labelled Close operation that RunParallel fires off
// in its own goroutine. Label is used to build the error message when
// Close returns non-nil or when the goroutine doesn't finish within
// the wall-clock timeout.
type Task struct {
	Label string
	Close func() error
}

// RunParallel invokes every Task concurrently and collects their
// errors, enforcing a single wall-clock timeout across the whole set.
// Tasks that do not finish in time are abandoned and reported as
// timeout errors so the caller still sees them in the joined output.
// Successful Tasks contribute nothing to the result.
func RunParallel(tasks []Task, timeout time.Duration) []error {
	if len(tasks) == 0 {
		return nil
	}
	type result struct {
		label string
		err   error
	}
	results := make(chan result, len(tasks))
	for _, t := range tasks {
		go func(t Task) {
			results <- result{t.Label, t.Close()}
		}(t)
	}

	var errs []error
	remaining := len(tasks)
	deadline := time.After(timeout)
	pending := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		pending[t.Label] = struct{}{}
	}
	for remaining > 0 {
		select {
		case r := <-results:
			remaining--
			delete(pending, r.label)
			if r.err != nil {
				errs = errorsx.Append(errs, errorsx.WrapLifecycle("close "+r.label, r.err))
			}
		case <-deadline:
			for label := range pending {
				errs = errorsx.Append(errs, fmt.Errorf("close %s: did not finish within %s", label, timeout))
			}
			return errs
		}
	}
	return errs
}
