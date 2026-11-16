package webview2host

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// paneEnvironment is the pane WebView2 environment's lifecycle, kept free of
// COM so it builds and is tested on every platform. E is the environment the
// Windows host adopts; start kicks off one generation's asynchronous create,
// which answers through completed, and free drops an environment's COM
// references.
//
// The environment is created lazily, once per generation, by the first
// create that needs it (acquire). release ends a generation: it frees the
// adopted environment, wakes every waiter with a retry error, and makes a
// late completion of the old generation free its own environment instead of
// adopting it. Clear-data releases unconditionally; releaseIdle releases only
// while no create holds the generation and the host has no page, so the
// backend's engine stop frees the browser process tree without stranding a
// page or a waiter.
//
// Threading: start, completed, settle, release and releaseIdle run on the UI
// thread, which orders them against each other; acquire runs on a directive
// goroutine. releaseIdle calls openPages with mu held, so the host's page
// lock is taken after mu and never the other way around.
type paneEnvironment[E any] struct {
	onMain  func(func())
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
	// holds counts creates between acquire and their release func. A create
	// registers its page before releasing its hold, so no hold and no page
	// means nothing is using the environment or about to.
	holds int
}

func newPaneEnvironment[E any](onMain func(func()), start func(gen uint64), free func(E), logf func(string, ...any), timeout time.Duration) *paneEnvironment[E] {
	return &paneEnvironment[E]{
		onMain: onMain, start: start, free: free, logf: logf, timeout: timeout,
		ready: make(chan struct{}),
	}
}

// errEnvironmentReleased is what a create parked on a generation that a
// clear-data released is told. Its directive is answered create-failed.
var errEnvironmentReleased = errors.New("browser host environment was released; retry the directive")

// acquire returns the current generation's environment, starting it if this
// is the first create since the last release and waiting for it to settle.
// On success the caller holds the generation until it calls done, which it
// must do once its page is registered or its create has failed; releaseIdle
// leaves a held generation alone.
func (p *paneEnvironment[E]) acquire() (env E, done func(), err error) {
	p.mu.Lock()
	ready, gen := p.ready, p.gen
	arm := !p.starting
	p.starting = true
	p.holds++
	p.mu.Unlock()
	if arm {
		p.onMain(func() { p.begin(gen) })
	}
	unhold := func() {
		p.mu.Lock()
		p.holds--
		p.mu.Unlock()
	}

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-ready:
	case <-timer.C:
		unhold()
		var zero E
		return zero, nil, fmt.Errorf("browser host environment did not start within %s", p.timeout)
	}
	p.mu.Lock()
	switch {
	case p.gen != gen:
		err = errEnvironmentReleased
	case p.err != nil:
		err = p.err
	case !p.adopted:
		err = errors.New("browser host environment settled with no environment")
	default:
		env = p.env
	}
	p.mu.Unlock()
	if err != nil {
		unhold()
		var zero E
		return zero, nil, err
	}
	return env, sync.OnceFunc(unhold), nil
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
// folder a clear may have deleted, the second would leak its references.
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
// only a generation with no environment, so a duplicate create's failure
// cannot poison an adopted one.
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

// release ends the current generation whatever holds it. Clear-data uses it
// after closing every page: a parked create learns to retry, and a create in
// flight frees its environment on completion.
func (p *paneEnvironment[E]) release() {
	p.mu.Lock()
	env, adopted := p.endLocked()
	stale := p.rearmLocked()
	p.mu.Unlock()
	closeOnce(stale)
	if adopted {
		p.free(env)
	}
}

// releaseIdle ends the current generation only when nothing could be using
// it: no create holds it and openPages reports none. It reports whether it
// released, and otherwise why not. With no environment and no create in
// flight there is nothing to release, so a repeated release changes nothing.
func (p *paneEnvironment[E]) releaseIdle(openPages func() int) (released bool, reason string) {
	p.mu.Lock()
	switch {
	case !p.starting:
		p.mu.Unlock()
		return false, "no environment"
	case p.holds > 0:
		holds := p.holds
		p.mu.Unlock()
		return false, fmt.Sprintf("%d create(s) in progress", holds)
	}
	if n := openPages(); n > 0 {
		p.mu.Unlock()
		return false, fmt.Sprintf("%d page(s) open", n)
	}
	env, adopted := p.endLocked()
	stale := p.rearmLocked()
	p.mu.Unlock()
	closeOnce(stale)
	if adopted {
		p.free(env)
	}
	return true, ""
}

// current returns the adopted environment, if any. Runs on the UI thread.
func (p *paneEnvironment[E]) current() (E, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.env, p.adopted
}

func (p *paneEnvironment[E]) endLocked() (E, bool) {
	env, adopted := p.env, p.adopted
	var zero E
	p.env, p.adopted, p.err = zero, false, nil
	return env, adopted
}

// rearmLocked moves to the next generation and returns the old generation's
// ready channel for the caller to close outside the lock. The superseded
// generation's settle no longer closes anything, so a waiter parked on it
// would otherwise sit out its whole timeout to learn it must retry.
func (p *paneEnvironment[E]) rearmLocked() chan struct{} {
	p.starting = false
	p.gen++
	stale := p.ready
	p.ready = make(chan struct{})
	return stale
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
