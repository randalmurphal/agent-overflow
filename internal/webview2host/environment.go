package webview2host

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// paneEnvironment is the pane WebView2 environment's generation contract,
// kept free of COM so it builds and is tested on every platform. E is the
// environment the Windows host adopts; start kicks off one generation's
// asynchronous create, which answers through completed or settle, and free
// drops an environment's COM references.
//
// The environment is created lazily, once per generation, by the first
// create that needs it (request). Clear-data's release ends a generation: it
// frees the adopted environment, wakes every waiter with a retry error, and
// makes a late completion of the old generation free its own environment
// instead of adopting it. A start queued for a generation that a release
// ended is skipped, so each generation starts at most one create and its
// outcome is that create's.
//
// Threading: begin, start, completed, settle and release run on the UI
// thread, which orders them. A directive goroutine waits in await.
type paneEnvironment[E any] struct {
	start   func(gen uint64)
	free    func(E)
	logf    func(string, ...any)
	timeout time.Duration

	mu sync.Mutex
	// ready is closed once the generation settles, success or failure, or
	// when release ends it.
	ready    chan struct{}
	gen      uint64
	starting bool
	env      E
	adopted  bool
	err      error
}

func newPaneEnvironment[E any](start func(gen uint64), free func(E), logf func(string, ...any), timeout time.Duration) *paneEnvironment[E] {
	return &paneEnvironment[E]{
		start: start, free: free, logf: logf, timeout: timeout,
		ready: make(chan struct{}),
	}
}

// errEnvironmentReleased is what a create parked on a generation that a
// clear-data released receives.
var errEnvironmentReleased = errors.New("browser host environment was cleared; retry the directive")

type environmentRequest struct {
	ready <-chan struct{}
	gen   uint64
}

// request reserves a generation and identifies the caller that starts it.
func (p *paneEnvironment[E]) request() (environmentRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	req := environmentRequest{ready: p.ready, gen: p.gen}
	arm := !p.starting
	p.starting = true
	return req, arm
}

func (p *paneEnvironment[E]) await(req environmentRequest) (E, error) {
	var zero E
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-req.ready:
	case <-timer.C:
		return zero, fmt.Errorf("browser host environment did not start within %s", p.timeout)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.gen != req.gen:
		return zero, errEnvironmentReleased
	case p.err != nil:
		return zero, p.err
	case !p.adopted:
		return zero, errors.New("browser host environment settled with no environment")
	}
	return p.env, nil
}

// begin starts gen's create unless a release ended gen while the start was
// queued. That release already woke gen's waiters and re-armed the start.
func (p *paneEnvironment[E]) begin(gen uint64) {
	p.mu.Lock()
	current := p.gen == gen
	p.mu.Unlock()
	if current {
		p.start(gen)
	}
}

// completed adopts gen's environment, or settles gen with err. An
// environment for a released generation, or a second one for an adopted
// generation, is freed rather than adopted: the first would be rooted in a
// folder a clear deleted, the second would leak its references.
func (p *paneEnvironment[E]) completed(gen uint64, env E, err error) {
	if err != nil {
		p.settle(gen, err)
		return
	}
	p.mu.Lock()
	stale := p.gen != gen || p.adopted
	if !stale {
		p.env, p.adopted = env, true
	}
	p.mu.Unlock()
	if stale {
		p.free(env)
		return
	}
	p.settle(gen, nil)
}

// settle records gen's outcome and releases its waiters. An error settles
// only a generation with no environment.
func (p *paneEnvironment[E]) settle(gen uint64, err error) {
	p.mu.Lock()
	if p.gen != gen {
		p.mu.Unlock()
		return
	}
	if p.err == nil && !p.adopted {
		p.err = err
	}
	ready := p.ready
	p.mu.Unlock()
	if err != nil {
		p.logf("browser host: %v", err)
	}
	closeOnce(ready)
}

// release ends the current generation and frees its environment, so the next
// create builds a fresh one. A waiter parked on the old generation is woken
// to retry rather than sitting out its timeout.
func (p *paneEnvironment[E]) release() {
	p.mu.Lock()
	env, adopted := p.env, p.adopted
	var zero E
	p.env, p.adopted, p.err = zero, false, nil
	p.starting = false
	p.gen++
	stale := p.ready
	p.ready = make(chan struct{})
	p.mu.Unlock()
	closeOnce(stale)
	if adopted {
		p.free(env)
	}
}

// current returns the adopted environment, if any.
func (p *paneEnvironment[E]) current() (E, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.env, p.adopted
}

// closeOnce closes ch unless it is closed. Only the UI thread closes ready
// channels, which is what makes the check race-free.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}
