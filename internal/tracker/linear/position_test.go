package linear

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func tieIssues(n int, at time.Time) []Issue {
	out := make([]Issue, n)
	for i := range out {
		out[i] = Issue{ID: fmt.Sprintf("issue-%04d", i), UpdatedAt: at}
	}
	return out
}

// A page whose tie outgrows the cohort delivers up to the cohort's bound
// and starts the drain at the next issue of the tie, the cohort kept for
// the drain to leave out; the Position it started from is unchanged,
// spare capacity included.
func TestAdvanceStartsTheDrain(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cohort := make([]string, maxCohort-1, maxCohort+5)
	for i := range cohort {
		cohort[i] = fmt.Sprintf("held-%03d", i)
	}
	spare := cohort[:maxCohort]
	spare[maxCohort-1] = "spare"
	from := Position{Bound: at, At: cohort}
	next, n, err := advance(opIssues, from, oldestFirst, tieIssues(3, at))
	if err != nil || n != 1 || next.Drain == nil || *next.Drain != "" || len(next.At) != maxCohort || next.At[maxCohort-1] != "issue-0000" {
		t.Fatalf("next %d left out, drain %v, %d delivered, %v", len(next.At), next.Drain, n, err)
	}
	if len(from.At) != maxCohort-1 || spare[maxCohort-1] != "spare" {
		t.Error("advance wrote into the cohort it was given")
	}
	// A newer issue first starts a new tie, which fits.
	later := append([]Issue{{ID: "newer", UpdatedAt: at.Add(time.Second)}}, tieIssues(2, at.Add(time.Second))...)
	next, n, err = advance(opIssues, from, oldestFirst, later)
	if err != nil || n != 3 || next.Drain != nil || !next.Bound.Equal(at.Add(time.Second)) || len(next.At) != 3 {
		t.Errorf("a new tie: %+v, %d delivered, %v", next, n, err)
	}
}

// A drain's page holds only the tie less its cohort; the drain goes on
// from the page's cursor, and once it ends the walk is past the bound.
func TestDrained(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	start := ""
	from := Position{Bound: at, At: []string{"held"}, Drain: &start}
	page := Page[Issue]{Items: tieIssues(2, at), HasNextPage: true, EndCursor: "c1"}
	next, err := drained(opIssues, from, page)
	if err != nil || next.Drain == nil || *next.Drain != "c1" || !slices.Equal(next.At, []string{"held"}) || next.Past {
		t.Fatalf("drain goes on: %+v %v", next, err)
	}
	page.HasNextPage = false
	if next, err = drained(opIssues, from, page); err != nil || next.Drain != nil || next.At != nil || !next.Past || !next.Bound.Equal(at) {
		t.Fatalf("drain ends: %+v %v", next, err)
	}
	for name, items := range map[string][]Issue{
		"outside the tie": {{ID: "x", UpdatedAt: at.Add(time.Millisecond)}},
		"left out":        {{ID: "held", UpdatedAt: at}},
	} {
		if _, err := drained(opIssues, from, Page[Issue]{Items: items}); err == nil {
			t.Errorf("an issue %s: no error", name)
		}
	}
	// Past the bound, an issue at it breaks the order.
	if _, _, err := advance(opIssues, Position{Bound: at, Past: true}, oldestFirst, tieIssues(1, at)); err == nil {
		t.Error("an issue at the bound of a walk past it: no error")
	}
}

// advance on a page of 250 issues tied at one time makes its cohort once
// and checks duplicates by a set: a few allocations, not one per issue.
func TestAdvanceAllocations(t *testing.T) {
	if raceOn {
		t.Skip("the race detector changes what allocates")
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := Position{Bound: at, At: []string{"held"}}
	tie, spread := tieIssues(250, at), tieIssues(250, at)
	for i := range spread {
		spread[i].UpdatedAt = at.Add(time.Duration(i) * time.Millisecond)
	}
	for name, items := range map[string][]Issue{"a tie": tie, "distinct times": spread} {
		allocs := testing.AllocsPerRun(20, func() {
			if _, _, err := advance(opIssues, from, oldestFirst, items); err != nil {
				t.Fatal(err)
			}
		})
		if allocs > advanceAllocs {
			t.Errorf("%s: %v allocations, pinned at most %d", name, allocs, advanceAllocs)
		}
	}
}

// advanceAllocs pins advance's allocations on a tie that starts the
// drain: the cohort's slice, the set and its buckets, and the drain's
// start (measured 5; 4 on distinct times).
const advanceAllocs = 5
