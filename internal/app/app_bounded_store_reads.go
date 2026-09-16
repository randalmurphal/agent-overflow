package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/transport"
)

// boundedStoreReadError maps the failure of a store read that ran under a
// deadline.
//
// An expired deadline is not the caller's mistake and the same call can
// succeed on retry, so it becomes transport.ErrTemporarilyUnavailable, which
// the dispatcher answers as `temporarily_unavailable` (503) instead of a
// plain method error. Every other failure keeps its own meaning under `what`.
//
// Each bounded read still names its own timeout constant, because the
// ceiling is a property of that call and its comment is where the choice is
// justified. Only this mapping is shared.
func boundedStoreReadError(ctx context.Context, what string, timeout time.Duration, err error) error {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf(
			"%w: %s timed out after %s: %w",
			transport.ErrTemporarilyUnavailable, what, timeout, ctxErr,
		)
	}
	return fmt.Errorf("%s: %w", what, err)
}
