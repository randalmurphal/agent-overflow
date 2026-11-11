package app

import (
	"context"
	"sync"
)

// workAdmission closes the gap between observing an idle host and handing it
// to the updater. Leases cover admission, not the lifetime of provider work:
// the existing runtime, queue and workflow owners answer whether work remains.
// Nested admissions are safe because quiescence requires zero leases.
type workAdmission struct {
	mu      sync.Mutex
	active  int
	resume  chan struct{}
	stopped bool
}

func (g *workAdmission) begin(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if g.stopped {
			g.mu.Unlock()
			return nil, ErrShuttingDown
		}
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		resume := g.resume
		if resume == nil {
			g.active++
			g.mu.Unlock()
			return g.end, nil
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-resume:
		}
	}
}

func (g *workAdmission) end() {
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
}

// check runs under the admission mutex: it must only read existing work owners,
// never invoke a provider, actor command, or another admission. A nonempty
// reason keeps the host open for normal work while the updater waits.
func (g *workAdmission) quiesce(check func() (string, error)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		return "Finishing current operations…", nil
	}
	if reason, err := check(); reason != "" || err != nil {
		return reason, err
	}
	if g.resume == nil {
		g.resume = make(chan struct{})
	}
	return "", nil
}

func (g *workAdmission) reopen() {
	g.mu.Lock()
	if g.resume != nil {
		close(g.resume)
		g.resume = nil
	}
	g.mu.Unlock()
}

// Reject callers parked behind a completed handoff before transport drains.
// They own no work and must not wait for the later app-lifetime cancellation.
// An ordinary shutdown with open admission retains its existing drain order.
func (g *workAdmission) stopWaiting() {
	g.mu.Lock()
	if g.resume != nil {
		g.stopped = true
		close(g.resume)
		g.resume = nil
	}
	g.mu.Unlock()
}
