package otel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// shutdownTimeout bounds how long we wait for exporters to flush. Five
// seconds is long enough for a well-behaved OTLP collector and short enough
// that a misconfigured endpoint doesn't block app exit.
const shutdownTimeout = 5 * time.Second

// Shutdown flushes and closes both the trace and metric exporters. It is
// safe to call on a nil or no-op provider. Errors from individual exporters
// are joined so the caller sees every failure, not just the first.
//
// Shutdown applies an internal 5-second deadline on top of whatever deadline
// the caller passes. This guarantees app exit doesn't block on a network
// flush for longer than that, which matters for user-facing Quit latency.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || !p.enabled || len(p.shutdownFns) == 0 {
		return nil
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	var errs []error
	for _, fn := range p.shutdownFns {
		if err := fn(deadlineCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				errs = append(errs, fmt.Errorf("otel: shutdown deadline exceeded: %w", err))
				continue
			}
			errs = append(errs, fmt.Errorf("otel: shutdown: %w", err))
		}
	}
	p.shutdownFns = nil
	return errors.Join(errs...)
}
