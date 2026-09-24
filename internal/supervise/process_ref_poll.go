//go:build !windows

package supervise

import (
	"context"
	"time"
)

func waitForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	return pollForExit(ctx, r, timeout)
}
