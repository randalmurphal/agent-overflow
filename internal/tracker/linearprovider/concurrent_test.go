package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// One Provider serves reads and writes from many goroutines at once; run
// under -race.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	const n = 16
	var script []lineartest.Exchange
	for i := 1; i <= n; i++ {
		script = append(script,
			updateX(issueID(i), fmt.Sprintf(`{"title":"Title %d"}`, i), applied(issue(i, 1).JSON())),
			issuesX(byIDVars([]string{issueID(i)}, nil, true), issue(i, float64(i))),
		)
	}
	p, _ := newProvider(t, script...)
	var wg sync.WaitGroup
	errs := make(chan string, 2*n)
	for i := 1; i <= n; i++ {
		id := tracker.IssueID(issueID(i))
		wg.Go(func() {
			r := p.Edit(context.Background(), tracker.EditInput{Issue: id,
				Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues(fmt.Sprintf("Title %d", i))}}})
			if !r.AllApplied() {
				errs <- describe(r)
			}
		})
		wg.Go(func() {
			c, err := p.Cards(context.Background(), []tracker.IssueID{id})
			if err != nil || len(c.Cards) != 1 || c.Cards[0].ID != id {
				errs <- fmt.Sprintf("cards %+v %v", c, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// A call cancelled while its request is in flight: a read fails with the
// context's error; a write's outcome is unknown, since Linear may have
// applied it.
func TestCancelInFlight(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 2)
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Wait: true, Arrived: arrived},
		issueStatesX(issueID(1), "state-todo", lineartest.State{ID: "s-done", Name: "Done", Type: "completed", Team: web}),
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"` + issueID(1) + `","input":{"stateId":"s-done"}}`, Wait: true, Arrived: arrived},
	)
	run := func(call func(ctx context.Context)) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); call(ctx) }()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the request never arrived")
		}
		cancel()
		<-done
	}
	run(func(ctx context.Context) {
		if _, err := p.Projects(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled read: %v", err)
		}
	})
	run(func(ctx context.Context) {
		r := p.Move(ctx, tracker.MoveInput{Issue: tracker.IssueID(issueID(1)), Move: "s-done"})
		expectResult(t, r, "1:move:unknown")
	})
}
