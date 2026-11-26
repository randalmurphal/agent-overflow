package linear

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
)

// Walks in update order past a tie larger than a cohort, against the
// stateful fake: tieSize issues updated at one time, others a minute
// apart before and after it.
const tieSize = 1250

var tieAt = cfStart.Add(-12 * time.Hour)

// tieLayout is how many issues come before and after the tie in update
// order, besides the conformance workspace's three, which are older.
type tieLayout struct{ earlier, later int }

// tieLater is the layout of most tests: three issues after the tie.
var tieLater = tieLayout{later: 3}

func tieID(i int) string { return fid(0x1000 + i) }

// tieWorkspace is the conformance workspace with the tie's issues as
// tieID 0 to tieSize-1, then l.later issues after it and l.earlier
// before it, created a minute apart in that order.
func tieWorkspace(l tieLayout) trackerfake.LinearWorkspace {
	w := conformanceWorkspace()
	for i := range tieSize + l.later + l.earlier {
		at := tieAt
		switch {
		case i >= tieSize+l.later:
			at = tieAt.Add(-time.Duration(i-tieSize-l.later+1) * time.Minute)
		case i >= tieSize:
			at = tieAt.Add(time.Duration(i-tieSize+1) * time.Minute)
		}
		w.Issues = append(w.Issues, trackerfake.LinearIssue{
			ID: tieID(i), Team: cfTeam, Number: float64(10 + i), Title: fmt.Sprintf("Issue %d", i), State: cfTodo, Creator: cfUser1,
			SortOrder: float64(1000 + i), CreatedAt: cfStart.Add(-40*cfDay + time.Duration(i)*time.Minute), UpdatedAt: at,
		})
	}
	return w
}

// walkStep reads the page after a Position: poll or newest first.
type walkStep func(from Position) (Page[Issue], Position)

func pollStep(t *testing.T, f *conformanceEnv) walkStep {
	return func(from Position) (Page[Issue], Position) {
		t.Helper()
		p := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: PollCursor{Position: from}})).must(t)
		return p.Page, p.Next.Position
	}
}

func recentStep(t *testing.T, f *conformanceEnv) walkStep {
	return func(from Position) (Page[Issue], Position) {
		t.Helper()
		p := answered(f.c.RecentIssues(f.ctx, RecentQuery{Filter: IssueFilter{Teams: []string{cfTeam}}, From: from})).must(t)
		issues := Page[Issue]{EndCursor: p.EndCursor, HasNextPage: p.HasNextPage, Usage: p.Usage, Served: p.Served}
		for _, h := range p.Items {
			issues.Items = append(issues.Items, h.Issue)
		}
		return issues, p.Next
	}
}

// walkFrom reads from from to the walk's end, calling between after each
// page, and returns each issue's update times as delivered and the
// Position stored before each page.
func walkFrom(t *testing.T, step walkStep, from Position, between func(page Page[Issue], next Position)) (map[string][]time.Time, []Position) {
	t.Helper()
	seen := map[string][]time.Time{}
	var stored []Position
	for range 40 {
		stored = append(stored, from)
		page, next := step(from)
		if page.HasNextPage && len(page.Items) == 0 {
			t.Fatalf("from %+v: a page with no issues says more follow", from)
		}
		for _, is := range page.Items {
			seen[is.ID] = append(seen[is.ID], is.UpdatedAt)
		}
		if between != nil {
			between(page, next)
		}
		from = next
		if !page.HasNextPage {
			return seen, stored
		}
	}
	t.Fatal("the walk did not end")
	return nil, nil
}

var tieSteps = map[string]func(*testing.T, *conformanceEnv) walkStep{"oldest first": pollStep, "newest first": recentStep}

// Untouched, a walk in either order delivers every issue once, the tie
// drained past its cohort, and a walk restarted from any Position it
// stored delivers exactly what the walk delivered after it. With a full
// page of 250 before the tie's issue past the cohort, the walk's first
// read of that issue delivers nothing and the drain's first page is read
// in the same call: no page that says more follow is empty.
func TestTieLargerThanCohortRestarts(t *testing.T) {
	t.Parallel()
	for name, layout := range map[string]map[string]tieLayout{
		"oldest first": {"later issues": tieLater, "a page that fills the cohort": {earlier: 250 - maxCohort - 3}},
		"newest first": {"later issues": tieLater, "a page that fills the cohort": {later: 250 - maxCohort}},
	} {
		for what, l := range layout {
			t.Run(name+", "+what, func(t *testing.T) {
				t.Parallel()
				tieWalkRestarts(t, tieSteps[name], l)
			})
		}
	}
}

func tieWalkRestarts(t *testing.T, mk func(*testing.T, *conformanceEnv) walkStep, l tieLayout) {
	t.Helper()
	step := mk(t, newFakeEnv(t, tieWorkspace(l)))
	var pages [][]string
	seen, stored := walkFrom(t, step, Position{}, func(page Page[Issue], _ Position) {
		var ids []string
		for _, is := range page.Items {
			ids = append(ids, is.ID)
		}
		pages = append(pages, ids)
	})
	if all := tieSize + l.earlier + l.later + 3; len(seen) != all {
		t.Fatalf("%d issues delivered, want %d", len(seen), all)
	}
	for id, times := range seen {
		if len(times) != 1 {
			t.Fatalf("issue %s delivered %d times", id, len(times))
		}
	}
	if !slices.ContainsFunc(stored, func(p Position) bool { return p.Drain != nil }) {
		t.Fatal("the walk never drained the tie")
	}
	for i, from := range stored {
		again, _ := walkFrom(t, step, from, nil)
		want := slices.Sorted(slices.Values(slices.Concat(pages[i:]...)))
		if got := slices.Sorted(maps.Keys(again)); !slices.Equal(got, want) {
			t.Fatalf("restarted from stored Position %d (%+v): %d issues, want %d", i, from, len(got), len(want))
		}
	}
}

// Issues updated and trashed while the tie drains, among them the last
// issue of the drain's first page, whose cursor the drain goes on from:
// every untouched issue comes once at its time. Oldest first, an updated
// issue comes again at its new time, one updated before the drain reached
// it once; newest first, an update moves an issue behind the walk, so it
// never comes again. A trashed issue leaves both walks.
func TestTieDrainWithChanges(t *testing.T) {
	t.Parallel()
	for name, mk := range tieSteps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeEnv(t, tieWorkspace(tieLater))
			delivered := map[string]bool{}
			want := map[string][]time.Time{}
			drainPages := 0
			seen, _ := walkFrom(t, mk(t, f), Position{}, func(page Page[Issue], next Position) {
				for _, is := range page.Items {
					delivered[is.ID] = true
				}
				if next.Drain == nil {
					return
				}
				if drainPages++; drainPages != 2 {
					return
				}
				// After the drain's first page: update an issue it delivered
				// and one it has yet to; trash its cursor's issue and one it
				// has yet to.
				f.clock.advance(time.Minute)
				first, last := page.Items[0], page.Items[len(page.Items)-1]
				var ahead []string
				for i := range tieSize {
					if id := tieID(i); !delivered[id] {
						ahead = append(ahead, id)
					}
				}
				var at []time.Time
				for _, id := range []string{first.ID, ahead[0]} {
					at = append(at, answered(f.c.UpdateIssue(f.ctx, id, UpdateIssueInput{Title: Set("Changed during the drain")})).must(t).UpdatedAt)
				}
				for _, id := range []string{last.ID, ahead[1]} {
					if err := f.c.DeleteIssue(f.ctx, id, false); err != nil {
						t.Fatal(err)
					}
				}
				want[first.ID], want[ahead[0]], want[last.ID], want[ahead[1]] = []time.Time{tieAt}, nil, []time.Time{tieAt}, nil
				if name == "oldest first" {
					want[first.ID], want[ahead[0]] = []time.Time{tieAt, at[0]}, at[1:]
				}
			})
			if drainPages < 2 || len(want) != 4 {
				t.Fatalf("the walk read %d drain pages", drainPages)
			}
			for i := range tieSize + tieLater.later {
				id := tieID(i)
				w, ok := want[id]
				if !ok {
					w = []time.Time{tieAt}
					if i >= tieSize {
						w = []time.Time{tieAt.Add(time.Duration(i-tieSize+1) * time.Minute)}
					}
				}
				if !slices.EqualFunc(seen[id], w, time.Time.Equal) {
					t.Errorf("issue %s delivered at %v, want %v", id, seen[id], w)
				}
			}
		})
	}
}
