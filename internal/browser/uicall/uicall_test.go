package uicall

import (
	"testing"
	"time"
)

// An action whose dispatch times out before it starts never runs, so the
// error its caller reports stays true; one that times out while it runs is
// waited for and answered as run; one that runs in time is answered as run.
func TestRunNeverRunsAnActionAfterItsTimeout(t *testing.T) {
	var queued func()
	timedOut := func(fn func()) bool { queued = fn; return false }
	ran := false
	if got := Run(timedOut, func() { ran = true }); got != Withdrawn {
		t.Fatalf("an action whose dispatch timed out before it started answered %v", got)
	}
	queued()
	if ran {
		t.Fatal("an action answered as withdrawn ran")
	}

	started, release := make(chan struct{}), make(chan struct{})
	ran = false
	answered := make(chan Outcome, 1)
	go func() {
		answered <- Run(func(fn func()) bool {
			go fn()
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
	if got := <-answered; got != Ran || !ran {
		t.Fatalf("an action that started as its dispatch timed out answered %v, ran %v", got, ran)
	}

	if got := Run(func(fn func()) bool { fn(); return true }, func() {}); got != Ran {
		t.Fatalf("an action that ran in time answered %v", got)
	}
}
