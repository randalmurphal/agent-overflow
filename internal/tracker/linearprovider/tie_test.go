package linearprovider

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// A sync walk and a listing past a tie larger than the client's cohort,
// against the stateful fake: tieSize issues updated at one time and
// tieLater updated after it, every page read by a new Provider as a
// process restarted after each stored page would, the cursor's or
// token's bytes alone carried over.
const (
	tieSize  = 1250
	tieLater = 3
	tieAll   = tieSize + tieLater + 3 // and the scenario's own issues
)

var tieAt = scStart.Add(-12 * time.Hour)

func tieID(i int) string { return sid(0x1000 + i) }

func tieScenario(t *testing.T) *scenario {
	t.Helper()
	s := newScenario(t)
	ws := scenarioWorkspace()
	for i := range tieSize + tieLater {
		at := tieAt
		if i >= tieSize {
			at = tieAt.Add(time.Duration(i-tieSize+1) * time.Minute)
		}
		ws.Issues = append(ws.Issues, trackerfake.LinearIssue{
			ID: tieID(i), Team: scTeam, Number: float64(10 + i), Title: fmt.Sprintf("Issue %d", i), State: scTodo, Creator: scUser1,
			SortOrder: float64(1000 + i), CreatedAt: scStart.Add(-40*24*time.Hour + time.Duration(i)*time.Minute), UpdatedAt: at,
		})
	}
	if err := s.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
		t.Fatal(err)
	}
	s.clock.advance(time.Minute)
	return s
}

// tieStep reads the page after from, the bytes of a cursor or token: the
// issues it delivered, the next bytes, whether more follow and whether
// the next bytes drain the tie.
type tieStep func(from []byte) (ids []string, next []byte, more, draining bool)

func (s *scenario) restarted(t *testing.T) *Provider {
	t.Helper()
	p, err := New(s.c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *scenario) changedStep(t *testing.T) tieStep {
	board := tracker.BoardID(scTeam)
	return func(from []byte) ([]string, []byte, bool, bool) {
		t.Helper()
		pg := scenarioMust[tracker.RowPage](t)(s.restarted(t).ChangedRows(s.ctx, tracker.DeltaQuery{Board: board, From: from, Start: scStart.Add(-30 * 24 * time.Hour)}))
		var ids []string
		for _, r := range pg.Rows {
			ids = append(ids, string(r.Card.ID))
		}
		cur, err := decodeCursor(board, pg.Next)
		if err != nil {
			t.Fatal(err)
		}
		return ids, pg.Next, pg.More, cur.Drain != nil
	}
}

func (s *scenario) listingStep(t *testing.T) tieStep {
	return func(from []byte) ([]string, []byte, bool, bool) {
		t.Helper()
		pg := scenarioMust[tracker.SearchPage](t)(s.restarted(t).Search(s.ctx, tracker.SearchQuery{Project: tracker.ProjectID(scTeam), Page: tracker.PageToken(from)}))
		var ids []string
		for _, c := range pg.Cards {
			ids = append(ids, string(c.ID))
		}
		if pg.Next == "" {
			return ids, nil, false, false
		}
		tok, err := decodeSearchToken(pg.Next, scTeam, false)
		if err != nil {
			t.Fatal(err)
		}
		return ids, []byte(pg.Next), true, tok.Drain != nil
	}
}

// tieWalk reads from from to the walk's end, calling between after each
// page with its issues and whether the next bytes drain the tie. It
// returns how often each issue was delivered, each page's issues, the
// bytes stored before each page and the last.
func tieWalk(t *testing.T, step tieStep, from []byte, between func(ids []string, draining bool)) (seen map[string]int, pages [][]string, stored [][]byte, last []byte) {
	t.Helper()
	seen = map[string]int{}
	for range 60 {
		stored = append(stored, from)
		ids, next, more, draining := step(from)
		if more && len(ids) == 0 {
			t.Fatalf("from %s: a page with no issues says more follow", from)
		}
		for _, id := range ids {
			seen[id]++
		}
		pages = append(pages, ids)
		if between != nil {
			between(ids, draining)
		}
		from = next
		if !more {
			return seen, pages, stored, from
		}
	}
	t.Fatal("the walk did not end")
	return
}

var tieSteps = map[string]func(*scenario, *testing.T) tieStep{"sync": (*scenario).changedStep, "listing": (*scenario).listingStep}

// Untouched, a sync walk and a listing each deliver every issue once,
// draining the tie; restarted from any bytes they stored, each delivers
// exactly what it delivered after them. The sync walk after delivers
// nothing in one page.
func TestTieAcrossRestarts(t *testing.T) {
	t.Parallel()
	for name, mk := range tieSteps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			step := mk(tieScenario(t), t)
			drained := false
			seen, pages, stored, last := tieWalk(t, step, nil, func(_ []string, draining bool) { drained = drained || draining })
			if len(seen) != tieAll || !drained {
				t.Fatalf("%d issues delivered, want %d; drained %t", len(seen), tieAll, drained)
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("issue %s delivered %d times", id, n)
				}
			}
			for i, from := range stored {
				again, _, _, _ := tieWalk(t, step, from, nil)
				want := slices.Sorted(slices.Values(slices.Concat(pages[i:]...)))
				if got := slices.Sorted(maps.Keys(again)); !slices.Equal(got, want) {
					t.Fatalf("restarted from stored bytes %d %s: %d issues, want %d", i, from, len(got), len(want))
				}
			}
			if name == "sync" {
				if seen, pages, _, _ := tieWalk(t, step, last, nil); len(seen) != 0 || len(pages) != 1 {
					t.Errorf("the next walk delivered %d in %d pages", len(seen), len(pages))
				}
			}
		})
	}
}

// Issues updated and trashed while the tie drains, among them the last
// issue of the drain's first page, whose cursor the drain goes on from:
// every untouched issue comes once. The sync walk delivers an updated
// issue again at its new time, an issue updated before the drain reached
// it once; the listing, which an update moves behind, never again. A
// trashed issue leaves both.
func TestTieDrainWithChanges(t *testing.T) {
	t.Parallel()
	for name, mk := range tieSteps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := tieScenario(t)
			delivered := map[string]bool{}
			want := map[string]int{}
			drainPages := 0
			seen, _, _, _ := tieWalk(t, mk(s, t), nil, func(ids []string, draining bool) {
				for _, id := range ids {
					delivered[id] = true
				}
				if !draining {
					return
				}
				if drainPages++; drainPages != 2 {
					return
				}
				// After the drain's first page: update an issue it delivered
				// and one it has yet to; trash its cursor's issue and one it
				// has yet to.
				s.clock.advance(time.Minute)
				var ahead []string
				for i := range tieSize {
					if id := tieID(i); !delivered[id] {
						ahead = append(ahead, id)
					}
				}
				for _, id := range []string{ids[0], ahead[0]} {
					if _, err := s.c.UpdateIssue(s.ctx, id, linear.UpdateIssueInput{Title: linear.Set("Changed during the drain")}); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range []string{ids[len(ids)-1], ahead[1]} {
					if err := s.c.DeleteIssue(s.ctx, id, false); err != nil {
						t.Fatal(err)
					}
				}
				want[ids[0]], want[ahead[0]], want[ids[len(ids)-1]], want[ahead[1]] = 1, 0, 1, 0
				if name == "sync" {
					want[ids[0]], want[ahead[0]] = 2, 1
				}
			})
			if drainPages < 2 || len(want) != 4 {
				t.Fatalf("the walk read %d drain pages", drainPages)
			}
			for i := range tieSize + tieLater {
				id := tieID(i)
				n, ok := want[id]
				if !ok {
					n = 1
				}
				if seen[id] != n {
					t.Errorf("issue %s delivered %d times, want %d", id, seen[id], n)
				}
			}
		})
	}
}
