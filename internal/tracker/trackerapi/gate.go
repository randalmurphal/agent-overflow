package trackerapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type interactiveKey struct{}

// WithInteractive marks ctx as carrying a person's own action rather than
// a poller's. Such a call waits for a closed rate gate when the wait is
// short instead of failing at once.
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive reports whether ctx, or a context it derives from, was
// marked by WithInteractive.
func IsInteractive(ctx context.Context) bool {
	marked, _ := ctx.Value(interactiveKey{}).(bool)
	return marked
}

// gate is the client's rate gate: closed after a rate limit until the
// time the tracker named. A new closure never shortens an open one.
type gate struct {
	mu    sync.Mutex
	until time.Time
}

func (g *gate) closedUntil(now time.Time) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until, now.Before(g.until)
}

// close closes the gate until until, or keeps a later closure, and
// returns when it opens.
func (g *gate) close(until time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until.After(g.until) {
		g.until = until
	}
	return g.until
}

// pacer spaces requests at least interval apart. One waiter holds the
// turn at a time; a waiter whose context ends leaves without taking a
// slot.
type pacer struct {
	interval time.Duration
	turn     chan struct{}
	last     time.Time // guarded by turn
}

func newPacer(interval time.Duration) *pacer {
	return &pacer{interval: interval, turn: make(chan struct{}, 1)}
}

func (p *pacer) wait(ctx context.Context, now func() time.Time, sleep func(context.Context, time.Duration) error) error {
	if p == nil {
		return nil
	}
	select {
	case p.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.turn }()
	if !p.last.IsZero() {
		if wait := p.last.Add(p.interval).Sub(now()); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	p.last = now()
	return nil
}

// admit passes the rate gate and the pacing, in that order, and checks
// the gate again after pacing, which another call's rate limit may have
// closed meanwhile. Its errors are *NotSentError.
func (c *Client) admit(ctx context.Context, k *call, state *retries) error {
	for {
		if err := c.waitGate(ctx, k, state); err != nil {
			return err
		}
		if err := c.pace.wait(ctx, c.now, c.sleep); err != nil {
			return &NotSentError{Err: c.ended(ctx, c.where(k.method, k.target))}
		}
		if _, closed := c.gate.closedUntil(c.now()); !closed {
			return nil
		}
	}
}

// waitGate returns once the gate is open. A background call fails at once
// while it is closed; an interactive one waits when the gate opens before
// its deadline and within what is left of interactiveGateWait, one budget
// for every wait of the call.
func (c *Client) waitGate(ctx context.Context, k *call, state *retries) error {
	for {
		now := c.now()
		until, closed := c.gate.closedUntil(now)
		if !closed {
			return nil
		}
		wait := until.Sub(now)
		if !IsInteractive(ctx) || wait > interactiveGateWait-state.gateWaited || pastDeadline(ctx, wait) {
			return &NotSentError{Err: &RateLimitedError{Tracker: c.name, Until: until}}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return &NotSentError{Err: c.ended(ctx, c.where(k.method, k.target))}
		}
		state.gateWaited += wait
	}
}

// pastDeadline reports whether waiting wait would outlast ctx.
func pastDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && wait > time.Until(deadline)
}

// backoff is the wait before retry n (from zero): 1s, doubling, capped.
func backoff(n int) time.Duration {
	if n >= 6 {
		return maxRetryWait
	}
	return min(time.Duration(1<<max(n, 0))*time.Second, maxRetryWait)
}

// retryAfter reads Retry-After: delay seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		// A delay past a day is the tracker's error, not a plan.
		return time.Duration(min(seconds, 86400)) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
