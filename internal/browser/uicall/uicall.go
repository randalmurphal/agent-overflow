// Package uicall is a native engine's only way onto its UI thread, and it
// answers how each action ended in a way the caller can report truthfully.
//
// The UI loop's own post (Wails' InvokeAsync) runs a closure some time
// later, and the loop can be busy past any deadline or gone. A caller that
// gave up at its deadline and reported a failure would see the action
// happen anyway, a result written after it read it, and a native reference
// made that nobody holds. So a Thread offers two calls, and each call site
// says which it is: Run, for an action whose answer the caller reports (a
// view operation, a constructor, a cancel), never runs it once it answered
// withdrawn; Release, for an action that gives up something already held
// (a view's close, a reference's unref, a decision's answer), is never
// withdrawn, because a release that never runs leaks.
//
// Only the engine's adapter, which builds the Thread from the loop's post,
// may call that post (the forbidigo rule in .golangci.yml).
//
// Stdlib-only.
package uicall

import (
	"sync"
	"time"
)

// Thread is a UI thread that actions are posted to, each call waiting for
// its action at most the thread's timeout.
type Thread struct {
	post    func(func())
	alive   func() bool
	timeout time.Duration
}

// NewThread answers the thread post runs closures on, in the order posted,
// while alive answers true; timeout bounds each call's wait.
func NewThread(post func(func()), alive func() bool, timeout time.Duration) *Thread {
	return &Thread{post: post, alive: alive, timeout: timeout}
}

// Alive answers whether the thread's loop is there to run anything: the
// difference between an action that will never run and one that has not
// run yet.
func (t *Thread) Alive() bool { return t.alive() }

// Outcome is how an action given to Run ended.
type Outcome int

const (
	// Ran: the action ran to its end, and what it wrote may be read.
	Ran Outcome = iota + 1
	// Withdrawn: the action did not start and never will.
	Withdrawn
)

func (o Outcome) String() string {
	switch o {
	case Ran:
		return "ran"
	case Withdrawn:
		return "withdrawn"
	}
	panic("uicall: unknown outcome")
}

// Run runs action on the thread and answers how it ended: withdrawn when
// the loop is gone or did not start it within the timeout, after which it
// never starts; ran once it ran, waiting past the timeout for one that
// started in time. What action writes may be read only when it ran, and a
// reference it made reaches the caller then, so nothing is made that the
// caller does not hold.
func (t *Thread) Run(action func()) Outcome {
	type state int
	const (
		queued state = iota
		started
		withdrawn
	)
	var mu sync.Mutex
	at := queued
	finished := make(chan struct{})
	if t.dispatch(func() {
		mu.Lock()
		if at == withdrawn {
			mu.Unlock()
			return
		}
		at = started
		mu.Unlock()
		defer close(finished)
		action()
	}) {
		return Ran
	}
	mu.Lock()
	if at == queued {
		at = withdrawn
		mu.Unlock()
		return Withdrawn
	}
	mu.Unlock()
	<-finished
	return Ran
}

// Release runs action, which gives up something the caller holds, on the
// thread, and answers whether it ran within the timeout. It is never
// withdrawn: when it answers false while the loop is there, action still
// runs later, so action writes only what the caller reads after a true
// answer, or sends it through a buffered channel. When the loop is gone
// nothing runs, and what action would have released goes with the process.
func (t *Thread) Release(action func()) bool {
	return t.dispatch(action)
}

// dispatch posts fn and answers whether it ran within the timeout; when it
// answers false, fn may still run later.
func (t *Thread) dispatch(fn func()) bool {
	if !t.alive() {
		return false
	}
	done := make(chan struct{})
	t.post(func() {
		defer close(done)
		fn()
	})
	timer := time.NewTimer(t.timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
