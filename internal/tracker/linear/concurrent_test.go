package linear

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// One client serves many goroutines at once: reads, writes and the
// usage they record.
func TestConcurrentCalls(t *testing.T) {
	t.Parallel()
	const n = 16
	var script []lineartest.Exchange
	for i := range n {
		id := fmt.Sprintf("WEB-%d", i)
		script = append(script,
			lineartest.Exchange{Op: "Viewer", Body: viewerData, Header: map[string]string{"X-Complexity": "2"}},
			lineartest.Exchange{Op: "UpdateIssue", Vars: fmt.Sprintf(`{"id":%q,"input":{"title":"Title %d"}}`, id, i),
				Body: issueResult(lineartest.IssueJSON("issue-"+id, id, "2026-11-02T10:00:00.000Z", 1))},
		)
	}
	c := newFake(t, script...).client(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Go(func() {
			_, err := c.Viewer(context.Background())
			errs <- err
			_ = c.Usage()
		})
		wg.Go(func() {
			got, err := c.UpdateIssue(context.Background(), fmt.Sprintf("WEB-%d", i), UpdateIssueInput{Title: Set(fmt.Sprintf("Title %d", i))})
			if err == nil && got.Identifier != fmt.Sprintf("WEB-%d", i) {
				err = fmt.Errorf("issue %s for WEB-%d", got.Identifier, i)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// A cancelled call returns at once with the context's error; a write
// cancelled after sending has an unknown outcome.
func TestCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	f := newFake(t,
		lineartest.Exchange{Op: "Viewer", Wait: true, Arrived: started},
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"title":"x"}}`, Wait: true, Arrived: started},
	)
	c := f.client(t)
	for _, call := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := c.Viewer(ctx); return err },
		func(ctx context.Context) error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{Title: Set("x")})
			return err
		},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- call(ctx) }()
		<-started
		cancel()
		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error %v, want context.Canceled", err)
		}
		if WriteNotApplied(err) {
			t.Errorf("a call cancelled after sending reports not applied: %v", err)
		}
	}
	if _, err := c.Viewer(cancelledContext()); !errors.Is(err, context.Canceled) {
		t.Errorf("a call on a cancelled context: %v", err)
	}
	c.Close()
	_, err := c.Teams(context.Background())
	if _, ok := errors.AsType[*trackerapi.NotSentError](err); !ok || errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a call after Close: %v", err)
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
