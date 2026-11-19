package linear

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// Usage follows every answer, failed ones included: a 429 reporting no
// requests left makes it report zero, and an answer without budget
// headers leaves the budgets as the last answer that carried them.
func TestUsageAfterFailedAnswers(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		lineartest.Exchange{Op: "Viewer", Body: viewerData, Header: map[string]string{
			"X-Complexity": "3", "X-Ratelimit-Requests-Remaining": "1", "X-Ratelimit-Requests-Reset": "4000000000000",
		}},
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Status: http.StatusBadGateway, Body: "<html>bad gateway</html>"},
		lineartest.Exchange{Op: "Viewer", Status: http.StatusTooManyRequests, Body: limitBody, Header: map[string]string{
			"Retry-After": "30", "X-Ratelimit-Requests-Remaining": "0", "X-Ratelimit-Requests-Reset": "4000000000000",
		}},
	)
	c := f.client(t)
	ctx := context.Background()
	if _, err := c.Viewer(ctx); err != nil {
		t.Fatal(err)
	}
	if u := c.Usage(); u.Requests.Remaining != 1 || u.Complexity != 3 {
		t.Fatalf("usage after the 200: %+v", u)
	}
	if _, err := c.Teams(ctx); err == nil {
		t.Fatal("the 502 succeeded")
	}
	if u := c.Usage(); !u.Requests.Known || u.Requests.Remaining != 1 || u.Complexity != 0 {
		t.Errorf("usage after an answer without budget headers: %+v", u)
	}
	if _, err := c.Viewer(ctx); !isRateLimited(err) {
		t.Fatalf("the 429: %v", err)
	}
	if u := c.Usage(); !u.Requests.Known || u.Requests.Remaining != 0 || u.Requests.Allows(1) {
		t.Errorf("usage after the 429: %+v", u)
	}
}

func isRateLimited(err error) bool {
	_, ok := errors.AsType[*trackerapi.RateLimitedError](err)
	return ok
}

// A page's Usage is its own answer's even when another call's answer
// arrives between its headers and its body: the client-wide Usage then
// holds the other answer, and the page must not take it from there.
func TestPageUsageIsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	page := lineartest.Data(lineartest.PageJSON([]string{lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)}, false, "c9"))
	vars := func(after string) string {
		return `{"first":100,"after":"` + after + `","sort":[{"manual":{"order":"Ascending"}}]}`
	}
	otherDone := make(chan struct{})
	headersSent := make(chan struct{})
	f := newFake(t,
		lineartest.Exchange{Op: "Issues", Vars: vars("a"), Body: page, Header: map[string]string{"X-Complexity": "11"},
			BeforeBody: func() { close(headersSent); <-otherDone }},
		lineartest.Exchange{Op: "Issues", Vars: vars("b"), Body: page, Header: map[string]string{"X-Complexity": "22"}},
	)
	c := f.client(t)
	ctx := context.Background()
	var first Page[Issue]
	var firstErr error
	var wg sync.WaitGroup
	wg.Go(func() { first, firstErr = c.ListIssues(ctx, IssueQuery{}, "a") })
	<-headersSent
	// The first answer's headers have reached the client once its
	// Usage reports them; only then is the second call made.
	for i := 0; c.Usage().Complexity != 11; i++ {
		if i == 5000 {
			t.Fatal("the first answer's headers never reached Usage")
		}
		time.Sleep(time.Millisecond)
	}
	second, err := c.ListIssues(ctx, IssueQuery{}, "b")
	close(otherDone)
	wg.Wait()
	if err != nil || firstErr != nil {
		t.Fatal(err, firstErr)
	}
	if first.Usage.Complexity != 11 || second.Usage.Complexity != 22 {
		t.Errorf("page usage %d and %d, want 11 and 22", first.Usage.Complexity, second.Usage.Complexity)
	}
}
