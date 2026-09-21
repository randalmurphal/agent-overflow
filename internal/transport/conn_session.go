package transport

import (
	"context"
	"time"
)

func (h *connHandler) checkSession() SessionStatus {
	if h.sessions == nil {
		return SessionStatus{Refusal: "unknown_session"}
	}
	return h.sessions.Check(h.profile.sessionID)
}

func (h *connHandler) endSession(ctx context.Context, reason string, cancel context.CancelFunc) {
	h.sub.Close()
	h.writeFrame(ctx, ServerFrame{Type: frameTypeSessionEnded, Error: AuthFailure(reason)})
	h.closeWithCause(closeCauseSessionEnded, cancel)()
}

// watchSession checks the current deadline on every wake. Renewal can extend
// that deadline without reconnecting. Periodic checks also bound recovery from
// wall-clock changes or timers delayed by platform suspension.
func (h *connHandler) watchSession(ctx context.Context, cancel context.CancelFunc) {
	recheck, lifetime := resolveWatchWindows(h.sessionRecheck, h.maxLifetime, h.profile.isLoopback, h.sessions != nil)
	var cap <-chan time.Time
	if lifetime > 0 {
		timer := time.NewTimer(lifetime)
		defer timer.Stop()
		cap = timer.C
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cap:
			h.closeWithCause(closeCauseLifetime, cancel)()
			return
		case <-timer.C:
			status := h.checkSession()
			if status.Refusal != "" {
				h.endSession(ctx, status.Refusal, cancel)
				return
			}
			wait := recheck
			if !status.Deadline.IsZero() {
				remaining := time.Until(status.Deadline)
				// Admission owns expiry. A clock/sample race must recheck, never
				// close a renewed session based on a previously observed deadline.
				if remaining <= 0 {
					remaining = time.Millisecond
				}
				if wait <= 0 || remaining < wait {
					wait = remaining
				}
			}
			if wait > 0 {
				timer.Reset(wait)
			}
		}
	}
}
