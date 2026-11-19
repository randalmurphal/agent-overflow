package linear

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// drain polls page by page, each page through a new client as a process
// restarted after every page would, until a walk ends. It returns every
// issue delivered with its update times, the last Next, and whether any
// page restarted.
func drain(t *testing.T, f *fake, from PollCursor) (map[string][]time.Time, PollCursor, bool) {
	t.Helper()
	seen := map[string][]time.Time{}
	restarted := false
	for range 10 {
		p, err := f.client(t).PollIssues(context.Background(), PollQuery{TeamID: "team-web", IncludeArchived: true, From: from})
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range p.Items {
			seen[issue.ID] = append(seen[issue.ID], issue.UpdatedAt)
		}
		if p.Next.Floor.Before(from.Floor) {
			t.Fatalf("Floor went back from %v to %v", from.Floor, p.Next.Floor)
		}
		restarted = restarted || p.Restarted
		from = p.Next
		if !p.HasNextPage {
			return seen, from, restarted
		}
	}
	t.Fatal("the walk did not end")
	return nil, from, false
}

// A run of 101 issues sharing one update time, more than a page, is
// drained across restarts: the stored cursor carries the walk past the
// first 100, and the next poll starts at that time, inclusive.
func TestPollCohortLargerThanAPage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	store := lineartest.NewPollStore("team-web", true, issuePage, since)
	for i := range 101 {
		store.Updated[fmt.Sprintf("%03d", i)] = at
	}
	f := newFake(t, store.Script(4)...)
	seen, next, restarted := drain(t, f, PollCursor{Since: since})
	if len(seen) != 101 || restarted {
		t.Fatalf("%d issues delivered, restarted %t", len(seen), restarted)
	}
	if next != (PollCursor{Since: at, Floor: at}) {
		t.Errorf("next %+v, want a poll from the cohort's time", next)
	}
	// The next poll redelivers the cohort, inclusive, and still drains.
	seen, _, _ = drain(t, f, next)
	if len(seen) != 101 {
		t.Errorf("the next poll delivered %d issues", len(seen))
	}
}

// When Linear refuses a stored cursor, the walk restarts at the Floor,
// inclusive, and still delivers every issue.
func TestPollRefusedCursorRestartsAtFloor(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	store := lineartest.NewPollStore("team-web", true, issuePage, since)
	for i := range 101 {
		store.Updated[fmt.Sprintf("%03d", i)] = at
	}
	store.Updated["early"] = since.Add(time.Minute)
	store.RefuseCursor = true
	// Page 1, the refused cursor, the restarted page 1, page 2.
	f := newFake(t, store.Script(4)...)
	seen, next, restarted := drain(t, f, PollCursor{Since: since})
	if len(seen) != 102 || !restarted {
		t.Fatalf("%d issues delivered, restarted %t", len(seen), restarted)
	}
	if next.Since != at || next.Floor != at {
		t.Errorf("next %+v", next)
	}
}

// An issue updated during the walk is delivered at its new position, and
// one updated after it was read is delivered again.
func TestPollIssueUpdatedDuringWalk(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	store := lineartest.NewPollStore("team-web", true, issuePage, since)
	for i := range 250 {
		store.Updated[fmt.Sprintf("%03d", i)] = base.Add(time.Duration(i) * time.Minute)
	}
	later := base.Add(24 * time.Hour)
	store.OnPage = func(n int, s *lineartest.PollStore) {
		if n == 2 {
			s.Updated["150"] = later                  // not read yet
			s.Updated["010"] = later.Add(time.Second) // read on page 1
		}
	}
	f := newFake(t, store.Script(3)...)
	seen, next, _ := drain(t, f, PollCursor{Since: since})
	if len(seen) != 250 {
		t.Errorf("%d distinct issues delivered, want 250", len(seen))
	}
	if got := seen["150"]; len(got) != 1 || !got[0].Equal(later) {
		t.Errorf("issue updated during the walk delivered at %v", got)
	}
	if got := seen["010"]; len(got) != 2 || !got[1].Equal(later.Add(time.Second)) {
		t.Errorf("issue updated after it was read delivered at %v", got)
	}
	if !next.Since.Equal(later.Add(time.Second)) || next.After != "" {
		t.Errorf("next %+v, want the newest update", next)
	}
}

// An empty poll keeps its bounds; with no team and no time it reads every
// issue. A cursor refused for another reason than the input is not
// retried.
func TestPollIssuesEmptyAndOtherErrors(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	nested := `{"data":null,"errors":[{"message":"bad","path":["page","nodes",0,"project"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	f := newFake(t,
		lineartest.Exchange{Op: "Issues", Vars: `{"first":100,"sort":[{"updatedAt":{"order":"Ascending"}}]}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":100,"sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":100,"after":"c1","sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, Body: nested},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":100,"after":"c2","sort":[{"updatedAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}}`, Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
	)
	c := f.client(t)
	p, err := c.PollIssues(context.Background(), PollQuery{})
	if err != nil || len(p.Items) != 0 || p.Next != (PollCursor{}) {
		t.Fatalf("poll %+v %v", p, err)
	}
	// A cursor stored without a Floor never restarts before its Since.
	p, err = c.PollIssues(context.Background(), PollQuery{From: PollCursor{Since: since}})
	if err != nil || p.Next != (PollCursor{Since: since, Floor: since}) {
		t.Fatalf("poll %+v %v", p, err)
	}
	for _, after := range []string{"c1", "c2"} {
		if _, err := c.PollIssues(context.Background(), PollQuery{From: PollCursor{Since: since, After: after}}); err == nil {
			t.Errorf("cursor %s: no error", after)
		}
	}
}
