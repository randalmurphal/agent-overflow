// Package uicall runs an action on a native UI thread through a dispatcher
// that answers within a deadline, and answers how the action ended in a way
// the caller can report truthfully.
//
// A native engine's dispatcher (gtkDo, wkDo) answers false when the UI
// thread did not run the closure in time, and the closure may still run
// later. For an action whose answer the caller reports (a download's
// cancel, a progress sample), that leaves "failed" followed by the action
// happening anyway, and a result written after the caller read it. Run
// closes both: a dispatch that times out before the action starts withdraws
// it, one that times out while it runs waits for it, and the action's
// results are read only once it ran.
//
// Stdlib-only.
package uicall

import "sync"

// Dispatch runs fn on the UI thread and answers whether it completed in
// time; when it answers false, fn may still run later.
type Dispatch func(fn func()) bool

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

// Run runs action through dispatch and answers how it ended.
func Run(dispatch Dispatch, action func()) Outcome {
	type state int
	const (
		queued state = iota
		started
		withdrawn
	)
	var mu sync.Mutex
	at := queued
	finished := make(chan struct{})
	if dispatch(func() {
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
