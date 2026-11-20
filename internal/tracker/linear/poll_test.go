package linear

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// drain polls page by page, each page through a new client as a process
// restarted after every page would, until a walk ends. It returns every
// issue delivered with its update times and the last Next.
func drain(t *testing.T, f *fake, from PollCursor) (map[string][]time.Time, PollCursor) {
	t.Helper()
	seen := map[string][]time.Time{}
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
		from = p.Next
		if !p.HasNextPage {
			return seen, from
		}
	}
	t.Fatal("the walk did not end")
	return nil, from
}

// The last issue of a page updated between pages, with a later update
// elsewhere, loses nothing: the next page asks from the floor again, so
// the issues between the moved issue's old and new places are delivered,
// and both updated issues come again at their new times. A walk paging
// by cursor skips those issues (TestLinearCursorFollowsItsIssue in the
// fake's tests).
func TestPollUpdateBetweenPagesLosesNothing(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	store := lineartest.NewPollStore("team-web", true, issuePage, since)
	for i := range 300 {
		store.Updated[fmt.Sprintf("%03d", i)] = base.Add(time.Duration(i) * time.Minute)
	}
	later := base.Add(24 * time.Hour)
	store.OnPage = func(n int, s *lineartest.PollStore) {
		if n == 2 {
			s.Updated["249"] = later                  // page 1's last issue
			s.Updated["010"] = later.Add(time.Second) // read on page 1
		}
	}
	f := newFake(t, store.Script(2)...)
	seen, next := drain(t, f, Restart(since))
	if len(seen) != 300 {
		t.Errorf("%d distinct issues delivered, want 300", len(seen))
	}
	for id, want := range map[string]time.Time{"249": later, "010": later.Add(time.Second)} {
		if got := seen[id]; len(got) != 2 || !got[1].Equal(want) {
			t.Errorf("issue %s delivered at %v, want again at %v", id, got, want)
		}
	}
	if !next.Floor.Equal(later.Add(time.Second)) || !slices.Equal(next.AtFloor, []string{"010"}) || next.Walk == nil || !next.Walk.Delivered {
		t.Errorf("next %+v, want the newest update", next)
	}
}

// More issues sharing one update time than a page holds are each
// delivered once, the cursor leaving out by ID those already delivered;
// the walk resumed after them delivers nothing in one request.
func TestPollCohortLargerThanAPage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	store := lineartest.NewPollStore("team-web", true, issuePage, since)
	for i := range 600 {
		store.Updated[fmt.Sprintf("%03d", i)] = at
	}
	f := newFake(t, store.Script(4)...)
	seen, next := drain(t, f, Restart(since))
	if len(seen) != 600 {
		t.Fatalf("%d issues delivered", len(seen))
	}
	for id, times := range seen {
		if len(times) != 1 {
			t.Fatalf("issue %s delivered %d times", id, len(times))
		}
	}
	if !next.Floor.Equal(at) || len(next.AtFloor) != 600 {
		t.Fatalf("next floor %v with %d left out", next.Floor, len(next.AtFloor))
	}
	seen, next = drain(t, f, next.Resume())
	if len(seen) != 0 || next.Walk.Delivered {
		t.Errorf("the resumed walk delivered %d issues", len(seen))
	}
}

// Idle polls resumed exactly deliver nothing, one request each, and
// carry Linear's clock at each walk's first page.
func TestPollIdleResumes(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store := lineartest.NewPollStore("team-web", true, issuePage, at)
	store.Updated["a"] = at
	clock := at.Add(time.Hour)
	store.Clock = func() time.Time { return clock }
	f := newFake(t, store.Script(4)...)
	seen, next := drain(t, f, Restart(at))
	if len(seen) != 1 || !next.Walk.Start.Equal(clock) {
		t.Fatalf("delivered %d, walk %+v", len(seen), next.Walk)
	}
	for tick := range 3 {
		clock = clock.Add(time.Minute)
		seen, next = drain(t, f, next.Resume())
		if len(seen) != 0 || next.Walk.Delivered || !next.Walk.Start.Equal(clock) {
			t.Errorf("tick %d: delivered %d, walk %+v", tick, len(seen), next.Walk)
		}
	}
}

// A cohort past maxCohort fails the poll with its size rather than
// growing every later request.
func TestPollCohortBound(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store := lineartest.NewPollStore("team-web", false, issuePage, at)
	for i := range maxCohort + 1 {
		store.Updated[fmt.Sprintf("%04d", i)] = at
	}
	f := newFake(t, store.Script(5)...)
	from := Restart(at)
	var err error
	for range 5 {
		var p PollPage
		p, err = f.client(t).PollIssues(context.Background(), PollQuery{TeamID: "team-web", From: from})
		if err != nil {
			break
		}
		from = p.Next
	}
	e, ok := errors.AsType[*CohortTooLargeError](err)
	if !ok || e.Count != maxCohort+1 || !e.At.Equal(at) {
		t.Fatalf("err %v", err)
	}
}

// An empty poll with no team and no time reads every issue and keeps
// its bounds; a page out of order, one listing an issue left out, and a
// left-out ID that is not one fail the call.
func TestPollIssuesEmptyAndErrors(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	vars := lineartest.PollVars("", false, issuePage, since, []string{"a"})
	old := lineartest.NewIssue("b", "WEB-2", lineartest.FormatTime(since.Add(-time.Second)), 1)
	again := lineartest.NewIssue("a", "WEB-1", lineartest.FormatTime(since), 1)
	f := newFake(t,
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"sort":[{"updatedAt":{"order":"Ascending"}}]}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Issues", Vars: vars, Body: lineartest.Data(lineartest.PageJSON([]string{old.JSON()}, false, ""))},
		lineartest.Exchange{Op: "Issues", Vars: vars, Body: lineartest.Data(lineartest.PageJSON([]string{again.JSON()}, false, ""))},
	)
	c := f.client(t)
	p, err := c.PollIssues(context.Background(), PollQuery{})
	if err != nil || len(p.Items) != 0 || !p.Next.Floor.IsZero() || p.Next.AtFloor != nil || p.Next.Walk == nil || p.Next.Walk.Delivered {
		t.Fatalf("poll %+v %v", p, err)
	}
	if p.Next.Walk.Start.IsZero() {
		t.Error("the walk's start is not the answer's Date")
	}
	from := PollCursor{Floor: since, AtFloor: []string{"a"}}
	for _, what := range []string{"older", "left out"} {
		if _, err := c.PollIssues(context.Background(), PollQuery{From: from}); err == nil {
			t.Errorf("a page listing an issue %s: no error", what)
		}
	}
	_, err = c.PollIssues(context.Background(), PollQuery{From: PollCursor{Floor: since, AtFloor: []string{""}}})
	if !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a bad left-out ID: %v", err)
	}
}
