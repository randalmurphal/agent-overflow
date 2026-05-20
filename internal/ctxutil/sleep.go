package ctxutil

import (
	"context"
	"time"
)

// Sleep blocks for d, returning true when the sleep completes and
// false when ctx is canceled first. Zero or negative d still honors
// an already-canceled ctx: a pre-canceled context returns false
// without sleeping. This matters for ticker loops that pass a list
// of intervals that may include zero — without the ctx check, a
// loop iteration after Cancel would continue rather than exit.
//
// Replaces two near-identical helpers (the original `sleepCtx` in
// `app_mcp_bindings.go` and `sleepWithContext` in
// `internal/provider/codex/collab_agents.go`) so a future bug in the
// ctx-cancel + zero-duration interaction only has to be fixed once.
func Sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
