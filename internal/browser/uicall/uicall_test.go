package uicall

import (
	"sync"
	"testing"
	"time"
)

// loop is a UI loop a test runs by hand: each post queues its closure
// until the test runs it.
type loop struct {
	mu     sync.Mutex
	queued []func()
	gone   bool
}

func (l *loop) post(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queued = append(l.queued, fn)
}

func (l *loop) alive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.gone
}

// drain runs what is queued, in order.
func (l *loop) drain() {
	l.mu.Lock()
	queued := l.queued
	l.queued = nil
	l.mu.Unlock()
	for _, fn := range queued {
		fn()
	}
}

const short = 10 * time.Millisecond

// An action the loop did not start within the timeout never runs, so the
// error its caller reports stays true; one that started in time is waited
// for past the timeout and answered as run; one that runs in time is
// answered as run.
func TestRunNeverRunsAnActionAfterItsTimeout(t *testing.T) {
	l := &loop{}
	thread := NewThread(l.post, l.alive, short)
	ran := false
	if got := thread.Run(func() { ran = true }); got != Withdrawn {
		t.Fatalf("an action the loop did not start in time answered %v", got)
	}
	l.drain()
	if ran {
		t.Fatal("an action answered as withdrawn ran")
	}

	started, release := make(chan struct{}), make(chan struct{})
	slow := NewThread(func(fn func()) { go fn() }, l.alive, short)
	ran = false
	answered := make(chan Outcome, 1)
	go func() {
		answered <- slow.Run(func() {
			close(started)
			<-release
			ran = true
		})
	}()
	<-started
	select {
	case got := <-answered:
		t.Fatalf("answered %v before the running action ended", got)
	case <-time.After(5 * short):
	}
	close(release)
	if got := <-answered; got != Ran || !ran {
		t.Fatalf("an action that started in time answered %v, ran %v", got, ran)
	}

	inline := NewThread(func(fn func()) { fn() }, l.alive, short)
	if got := inline.Run(func() {}); got != Ran {
		t.Fatalf("an action that ran in time answered %v", got)
	}
}

// With the loop gone, Run withdraws at once and Release runs nothing.
func TestAGoneLoopRunsNothing(t *testing.T) {
	l := &loop{gone: true}
	thread := NewThread(l.post, l.alive, short)
	if thread.Alive() {
		t.Fatal("a gone loop answered alive")
	}
	if got := thread.Run(func() { t.Error("ran on a gone loop") }); got != Withdrawn {
		t.Fatalf("a gone loop's Run answered %v", got)
	}
	if thread.Release(func() { t.Error("released on a gone loop") }) {
		t.Fatal("a gone loop's Release answered that it ran")
	}
	if len(l.queued) != 0 {
		t.Fatalf("a gone loop was posted %d closures", len(l.queued))
	}
}

// A release the loop did not run within the timeout answers false and
// still runs when the loop gets to it; one that runs in time answers true.
func TestAReleaseIsNeverWithdrawn(t *testing.T) {
	l := &loop{}
	thread := NewThread(l.post, l.alive, short)
	released := make(chan struct{}, 1)
	if thread.Release(func() { released <- struct{}{} }) {
		t.Fatal("a release the loop did not run answered that it ran")
	}
	l.drain()
	select {
	case <-released:
	default:
		t.Fatal("a release that timed out never ran")
	}
	inline := NewThread(func(fn func()) { fn() }, l.alive, short)
	if !inline.Release(func() {}) {
		t.Fatal("a release that ran in time answered that it did not")
	}
}

// A settled release answers once it ran, however long past the timeout the
// loop takes, and answers false only for a loop that went away with it
// still queued.
func TestASettledReleaseAnswersWhenItRan(t *testing.T) {
	l := &loop{}
	thread := NewThread(l.post, l.alive, short)
	answered := make(chan bool, 1)
	ran := false
	go func() { answered <- thread.Settle(func() { ran = true }) }()
	select {
	case <-answered:
		t.Fatal("the settle answered before its release ran")
	case <-time.After(10 * short):
	}
	l.drain()
	if !<-answered || !ran {
		t.Fatal("the settle did not answer that its release ran")
	}

	gone := &loop{}
	thread = NewThread(gone.post, gone.alive, short)
	go func() { answered <- thread.Settle(func() {}) }()
	time.Sleep(2 * short)
	gone.mu.Lock()
	gone.gone = true
	gone.mu.Unlock()
	if <-answered {
		t.Fatal("a settle whose loop went away answered that its release ran")
	}
}
