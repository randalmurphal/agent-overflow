package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// findCandidates is a find bar's candidate set, kept by applying answers
// the way the frontend does: a full answer replaces it, an incremental
// one drops every changed row and adds the candidates among them.
type findCandidates map[string][2]int

func (c findCandidates) apply(answer ThreadFindAnswer) findCandidates {
	if !answer.Incremental {
		c = findCandidates{}
	}
	for _, id := range answer.Changed {
		delete(c, id)
	}
	for _, item := range answer.Items {
		c[item.ID] = [2]int{item.TurnIndex, item.ItemIndex}
	}
	return c
}

func (c findCandidates) String() string {
	ids := make([]string, 0, len(c))
	for id := range c {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c[a][0] != c[b][0] {
			return c[a][0] - c[b][0]
		}
		return c[a][1] - c[b][1]
	})
	return fmt.Sprint(ids)
}

func mustFindThread(t *testing.T, s *Store, threadID, query string, limit int, since *ThreadFindSince) ThreadFindAnswer {
	t.Helper()
	answer, err := s.SearchThreadItems(context.Background(), threadID, query, TimelineSelection{}, limit, since)
	if err != nil {
		t.Fatalf("find %q: %v", query, err)
	}
	return answer
}

// TestSearchThreadItems_RefreshReadsWhatChanged: a refresh with the last
// answer's state reads only the rows written after it, and applying it
// gives the candidates a full read gives, through edits, appends and a
// row that stops matching.
func TestSearchThreadItems_RefreshReadsWhatChanged(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	for i, summary := range []string{"alpha match", "beta", "gamma match", "delta"} {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("r%d", i), i, summary, int64(100+i))
	}
	answer := mustFindThread(t, s, "t1", "match", 10, nil)
	held := findCandidates{}.apply(answer)
	if held.String() != "[r0 r2]" || answer.Incremental {
		t.Fatalf("first answer = %s incremental %v", held, answer.Incremental)
	}

	quiet := mustFindThread(t, s, "t1", "match", 10, &answer.Since)
	if !quiet.Incremental || len(quiet.Items) != 0 {
		t.Fatalf("a refresh with nothing written = %+v", quiet)
	}

	steps := []func(){
		func() {
			summary := "beta now a match"
			if _, err := s.UpdateItemFields("t1", "r1", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
			summary = "gamma no longer"
			if _, err := s.UpdateItemFields("t1", "r2", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		},
		func() { mustInsertItemForSearch(t, s, "t1", "r4", 4, "a new match", 200) },
		func() {
			if err := s.InsertItem(Item{ID: "r5", ThreadID: "t1", TurnIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "streaming", Summary: "mat"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendItemSummary("t1", "r5", "ch streamed", 300); err != nil {
				t.Fatal(err)
			}
		},
	}
	for i, step := range steps {
		step()
		refresh := mustFindThread(t, s, "t1", "match", 10, &answer.Since)
		if !refresh.Incremental {
			t.Fatalf("step %d: the refresh read in full", i)
		}
		if len(refresh.Changed) >= 6 {
			t.Errorf("step %d: the refresh re-read %d rows, more than the step wrote", i, len(refresh.Changed))
		}
		held = held.apply(refresh)
		full := findCandidates{}.apply(mustFindThread(t, s, "t1", "match", 10, nil))
		if held.String() != full.String() {
			t.Fatalf("step %d: refreshed candidates %s, a full read gives %s", i, held, full)
		}
		answer = refresh
	}
	if held.String() != "[r0 r1 r4 r5]" {
		t.Fatalf("final candidates = %s", held)
	}
}

// TestSearchThreadItems_RefreshReadsInFull: the refresh falls back to a
// full read whenever the last answer cannot be carried forward.
func TestSearchThreadItems_RefreshReadsInFull(t *testing.T) {
	seed := func(t *testing.T) (*Store, ThreadFindAnswer) {
		s := newTestStore(t)
		mustCreateThreadForSearch(t, s, "t1", "x")
		mustCreateThreadForSearch(t, s, "t2", "y")
		for i := 0; i < 4; i++ {
			mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("r%d", i), i, "match", int64(100+i))
		}
		return s, mustFindThread(t, s, "t1", "match", 3, nil)
	}
	for name, c := range map[string]func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer{
		"a capped answer": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			return mustFindThread(t, s, "t1", "match", 3, &since)
		},
		"another query": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped = false
			return mustFindThread(t, s, "t1", "matc", 3, &since)
		},
		"another limit": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped = false
			return mustFindThread(t, s, "t1", "match", 4, &since)
		},
		"another thread": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped = false
			return mustFindThread(t, s, "t2", "match", 3, &since)
		},
		"a delete": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped = false
			if err := s.DeleteThreadItem("t1", "r3"); err != nil {
				t.Fatal(err)
			}
			return mustFindThread(t, s, "t1", "match", 3, &since)
		},
		"another generation": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped, since.Generation = false, "restored"
			return mustFindThread(t, s, "t1", "match", 3, &since)
		},
		"more changes than the limit": func(t *testing.T, s *Store, since ThreadFindSince) ThreadFindAnswer {
			since.Capped = false
			for i := 10; i < 14; i++ {
				mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("r%d", i), i, "other", int64(100+i))
			}
			return mustFindThread(t, s, "t1", "match", 3, &since)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, first := seed(t)
			if !first.Capped || !first.Since.Capped {
				t.Fatalf("the seed answer is not capped: %+v", first)
			}
			if got := c(t, s, first.Since); got.Incremental {
				t.Fatalf("the refresh after %s was incremental: %+v", name, got)
			}
		})
	}
}

// A refresh reads the changed rows through the revision index, not by
// walking the thread.
func TestSearchThreadItems_RefreshWalksTheRevisionIndex(t *testing.T) {
	s := newTestStore(t)
	filter, args := threadFindFilter(timelineScope{}, findNeedle("needle"))
	plan := planText(explainPlan(t, s, threadFindChangedSQL(filter, 10), append(args, "t1", int64(5))...))
	if !strings.Contains(plan, "idx_items_thread_rev (thread_id=? AND rev>?)") {
		t.Fatalf("refresh plan does not seek the revision index:\n%s", plan)
	}
}
