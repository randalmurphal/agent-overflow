package trackerapp

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eventLog records what a service emits and reports, and wakes a test
// waiting for it: a test waits on what the service did, never on a
// guessed duration.
type eventLog struct {
	mu       sync.Mutex
	boards   []BoardEvent
	issues   []IssueEvent
	loopErrs []error
	// changed is closed and replaced on every record.
	changed chan struct{}
	// settled is closed when the first reads have settled; open until a
	// test closes it when hold is set.
	settled chan struct{}
	// settles counts the loop's settle waits.
	settles      atomic.Int64
	disconnected atomic.Bool
	// onEmit, when set, sees each event as it is emitted, before it is
	// recorded.
	onEmit atomic.Pointer[func(any)]
	// setups counts the service's setup change nudges.
	setups atomic.Int64
}

func newEventLog() *eventLog {
	settled := make(chan struct{})
	close(settled)
	return &eventLog{changed: make(chan struct{}), settled: settled}
}

func (l *eventLog) record(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f()
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *eventLog) board(e BoardEvent) {
	l.emitted(e)
	l.record(func() { l.boards = append(l.boards, e) })
}

func (l *eventLog) issue(e IssueEvent) {
	l.emitted(e)
	l.record(func() { l.issues = append(l.issues, e) })
}

func (l *eventLog) emitted(e any) {
	if f := l.onEmit.Load(); f != nil {
		(*f)(e)
	}
}
func (l *eventLog) loopFailed(err error) {
	l.record(func() { l.loopErrs = append(l.loopErrs, err) })
}

// holdSettle makes the first reads unsettled until the returned func runs.
func (l *eventLog) holdSettle() func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settled = make(chan struct{})
	ch := l.settled
	return func() { close(ch) }
}

func (l *eventLog) waitSettled(ctx context.Context) error {
	l.mu.Lock()
	ch := l.settled
	l.mu.Unlock()
	l.record(func() { l.settles.Add(1) })
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// snapshot is a copy of what was recorded.
func (l *eventLog) snapshot() ([]BoardEvent, []IssueEvent, []error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.boards), slices.Clone(l.issues), slices.Clone(l.loopErrs)
}

// waitFor waits until cond holds of what was recorded, re-checking on each
// record, or fails the test after a bound no healthy run reaches.
func (l *eventLog) waitFor(t testing.TB, what string, cond func(boards []BoardEvent, issues []IssueEvent, errs []error) bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		l.mu.Lock()
		ok := cond(l.boards, l.issues, l.loopErrs)
		ch := l.changed
		l.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			b, i, e := l.snapshot()
			t.Fatalf("waited for %s; boards %+v issues %+v errors %v", what, b, i, e)
		}
	}
}

// boardEvents waits for n board events and returns them.
func (l *eventLog) boardEvents(t testing.TB, n int) []BoardEvent {
	t.Helper()
	l.waitFor(t, "board events", func(b []BoardEvent, _ []IssueEvent, _ []error) bool { return len(b) >= n })
	b, _, _ := l.snapshot()
	return b
}

// issueEvents waits for n issue events and returns them.
func (l *eventLog) issueEvents(t testing.TB, n int) []IssueEvent {
	t.Helper()
	l.waitFor(t, "issue events", func(_ []BoardEvent, i []IssueEvent, _ []error) bool { return len(i) >= n })
	_, i, _ := l.snapshot()
	return i
}

// waitCalls waits until p made n calls of name.
func waitCalls(t testing.TB, p *scripted, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for p.count(name) < n {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d %s calls; %d made: %v", n, name, p.count(name), p.callsInOrder())
		}
		p.waitCall()
	}
}
