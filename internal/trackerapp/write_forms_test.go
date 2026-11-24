package trackerapp

import (
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A form read goes to the tracker, interactive, and settles the
// connection's problem with its outcome: a failure is the problem, a
// success clears it, and input the provider refused before sending leaves
// it as it was. A found card is named from the mirror where the answer
// names nothing.
func TestFormReadsSettleTheProblem(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.w.set(func(*scripted) {
		f.w.projects = func() ([]tracker.Project, error) {
			return nil, &trackerapi.TransientError{Err: errors.New("reset")}
		}
		f.w.search = func(tracker.SearchQuery) (tracker.SearchPage, error) {
			return tracker.SearchPage{Cards: []tracker.Card{card("10404", 1_799_000_000_000)}}, nil
		}
	})
	before := len(f.p.callsInOrder())
	_, err := f.svc.Projects(t.Context(), f.conn.ID)
	if _, ok := errors.AsType[*trackerapi.TransientError](err); !ok || f.problem() != FailureTransient {
		t.Fatalf("projects failed with %v, problem %q", err, f.problem())
	}
	if cf, ok := errors.AsType[*CallFailure](err); !ok || cf.Failure != ClassifyFailure(f.conn.Kind, cf.Err) || cf.Failure.Kind != FailureTransient {
		t.Fatalf("projects failed without the person's sentence: %v", err)
	}
	if _, err := f.svc.Search(t.Context(), f.conn.ID, SearchRequest{}); !errors.Is(err, tracker.ErrInvalidInput) || f.problem() != FailureTransient {
		t.Fatalf("a search of nothing: %v, problem %q", err, f.problem())
	}
	page, err := f.svc.Search(t.Context(), f.conn.ID, SearchRequest{Text: "web"})
	if err != nil || f.problem() != "" {
		t.Fatalf("search: %v, problem %q", err, f.problem())
	}
	if len(page.Cards) != 1 || page.Cards[0].Status.Name != "To Do" || page.Cards[0].Assignee == nil || page.Cards[0].Assignee.Name != "User 1" {
		t.Fatalf("page %+v", page)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Projects", "Search", "Search"}) {
		t.Fatalf("calls %v", got)
	}
	if slices.Contains(f.p.interactiveCalls()[before:], false) {
		t.Fatal("a form read was not interactive")
	}
}

// RefreshWorklogs reads an author's days from the time tracker in one
// read, makes them the mirror's and answers from the mirror; a failed read
// answers the days as the mirror held them with the failure, which is the
// connection's problem.
func TestRefreshWorklogs(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	u := tracker.UserID("acc-user1")
	f.w.time.logs = []tracker.Worklog{
		{ID: "w1", Issue: "10001", Author: u, Day: day(t, "2026-11-02"), Spent: time.Hour},
		{ID: "w2", Issue: "10002", Author: u, Day: day(t, "2026-11-30"), Spent: 30 * time.Minute},
		{ID: "w3", Issue: "10002", Author: u, Day: day(t, "2026-12-01"), Spent: time.Minute},
	}
	got := must[WorklogDays](t)(f.svc.RefreshWorklogs(t.Context(), f.conn.ID, "acc-user1", "2026-11-01", "2026-11-30"))
	if len(got.Worklogs) != 2 || got.Worklogs[0].ID != "w1" || got.Worklogs[1].SpentSeconds != 1800 || got.Failure != nil {
		t.Fatalf("worklogs %+v", got)
	}
	f.w.time.mu.Lock()
	reads := slices.Clone(f.w.time.reads)
	f.w.time.fail = &trackerapi.TransientError{Err: errors.New("reset")}
	f.w.time.mu.Unlock()
	if len(reads) != 1 || reads[0].From.String() != "2026-11-01" || reads[0].To.String() != "2026-11-30" {
		t.Fatalf("worklog reads %+v", reads)
	}
	failed := must[WorklogDays](t)(f.svc.RefreshWorklogs(t.Context(), f.conn.ID, "acc-user1", "2026-11-01", "2026-11-30"))
	if len(failed.Worklogs) != 2 || failed.Failure == nil || failed.Failure.Kind != string(FailureTransient) || f.problem() != FailureTransient {
		t.Fatalf("after a failed read: %+v, problem %q", failed, f.problem())
	}
	before := len(f.p.callsInOrder())
	for name, do := range map[string]func() error{
		"more than 31 days": func() error {
			_, err := f.svc.RefreshWorklogs(t.Context(), f.conn.ID, "acc-user1", "2026-11-01", "2026-12-02")
			return err
		},
		"days backwards": func() error {
			_, err := f.svc.RefreshWorklogs(t.Context(), f.conn.ID, "acc-user1", "2026-11-02", "2026-11-01")
			return err
		},
		"no author": func() error {
			_, err := f.svc.Worklogs(f.conn.ID, "", "2026-11-01", "2026-11-02")
			return err
		},
	} {
		if err := do(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := f.callsSince(before); len(got) > 0 {
		t.Fatalf("refused ranges called the tracker: %v", got)
	}
}

// On a tracker with no time tracking, a worklog write is refused as
// unsupported with nothing sent and the connection's problem left as it
// was, and a worklog refresh is refused by name.
func TestWorklogsWithNoTimeTracking(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.w.time = nil
	f.w.set(func(*scripted) {
		f.w.projects = func() ([]tracker.Project, error) { return nil, &trackerapi.TransientError{Err: errors.New("reset")} }
	})
	if _, err := f.svc.Projects(t.Context(), f.conn.ID); err == nil || f.problem() != FailureTransient {
		t.Fatalf("projects: %v, problem %q", err, f.problem())
	}
	before := len(f.p.callsInOrder())
	v, err := f.svc.AddWorklog(t.Context(), f.conn.ID, WorklogRequest{Issue: "10001", Day: "2026-11-02", SpentSeconds: 60})
	if err != nil || len(v.Parts) != 1 || v.Parts[0].Refusal == nil || v.Parts[0].Refusal.Reason != "unsupported" || v.Parts[0].Issue != "10001" {
		t.Fatalf("%+v, %v", v, err)
	}
	if _, err := f.svc.RefreshWorklogs(t.Context(), f.conn.ID, "acc-user1", "2026-11-01", "2026-11-02"); !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("refresh: %v", err)
	}
	if got := f.callsSince(before); len(got) > 0 || f.problem() != FailureTransient {
		t.Fatalf("calls %v, problem %q", got, f.problem())
	}
}
