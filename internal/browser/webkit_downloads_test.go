//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"testing"
	"time"
)

// A download action whose UI dispatch times out before it starts never
// runs, so the error its caller reports stays true; one that times out
// while it runs is waited for and answered as run.
func TestRunOrWithdrawNeverRunsAfterItsTimeout(t *testing.T) {
	// do queues fn for the UI thread and answers false, as a dispatch that
	// times out does; run runs what it queued.
	timedOut := func() (func(func()) bool, func()) {
		var queued func()
		return func(fn func()) bool { queued = fn; return false }, func() { queued() }
	}

	do, run := timedOut()
	ran := false
	if runOrWithdraw(do, func() { ran = true }) {
		t.Fatal("an action whose dispatch timed out before it started answered as run")
	}
	run()
	if ran {
		t.Fatal("an action whose dispatch timed out ran after it was answered as not run")
	}

	// The UI thread starts it as the dispatch times out.
	started, release := make(chan struct{}), make(chan struct{})
	var queued func()
	ran = false
	answered := make(chan bool, 1)
	go func() {
		answered <- runOrWithdraw(func(fn func()) bool {
			queued = fn
			go queued()
			<-started
			return false
		}, func() {
			close(started)
			<-release
			ran = true
		})
	}()
	select {
	case got := <-answered:
		t.Fatalf("answered %v before the running action ended", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if !<-answered || !ran {
		t.Fatal("an action that started as its dispatch timed out was not answered as run")
	}

	if !runOrWithdraw(func(fn func()) bool { fn(); return true }, func() {}) {
		t.Fatal("an action that ran answered as not run")
	}
}
