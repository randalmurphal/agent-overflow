package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestSearchHidesPreparingForkUntilPublication(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("pending", "claude")
	thread.Title = "quokka"
	thread.ForkPreparing = true
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	mustInsertItemForSearch(t, s, thread.ID, "item", 0, "quokka", 1)
	if hits, err := s.SearchThreadMessages("quokka", 10); err != nil || len(hits) != 0 {
		t.Errorf("global search exposed preparing fork: %+v err=%v", hits, err)
	}
	if _, err := s.SearchThreadItems(context.Background(), thread.ID, "quokka", TimelineSelection{}, 10, nil); !errors.Is(err, ErrForkPreparing) {
		t.Errorf("thread search accepted preparing fork: %v", err)
	}
	if _, err := s.FinishForkPreparation(thread.ID); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.SearchThreadMessages("quokka", 10); err != nil || len(hits) != 2 {
		t.Fatalf("ready fork search: %+v err=%v", hits, err)
	}
}

func mustCreateThreadForSearch(t *testing.T, s *Store, id, title string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: id, Title: title, Provider: "codex",
		WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

func mustInsertItemForSearch(t *testing.T, s *Store, threadID, itemID string, turnIndex int, summary string, createdAt int64) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Summary:   summary,
		CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("insert item %s: %v", itemID, err)
	}
}

// ---- Trivial inputs ----

func TestSearchThreadMessages_EmptyQueryReturnsNil(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "alpha")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "hello", 100)

	for name, query := range map[string]string{"empty": "", "whitespace only": "   \t\n  "} {
		t.Run(name, func(t *testing.T) {
			got, err := s.SearchThreadMessages(query, 10)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if got != nil {
				t.Errorf("expected nil hits for query %q, got %+v", query, got)
			}
		})
	}
}

func TestSearchThreadMessages_NoMatchReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "alpha")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "hello world", 100)

	got, err := s.SearchThreadMessages("nothing", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %+v", got)
	}
}

// ---- Title matches ----

func TestSearchThreadMessages_MatchOnTitle(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "Build the release pipeline")
	mustCreateThreadForSearch(t, s, "t2", "Fix typo in README")

	got, err := s.SearchThreadMessages("release", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ThreadID != "t1" {
		t.Fatalf("expected one hit for t1, got %+v", got)
	}
	if got[0].MatchType != "title" {
		t.Errorf("expected matchType=title, got %q", got[0].MatchType)
	}
	if got[0].ItemID != "" {
		t.Errorf("title match should not carry an item id, got %q", got[0].ItemID)
	}
}

func TestSearchThreadMessages_TitleMatchesAreDedupedAcrossItems(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "Build the release pipeline")
	for i := 0; i < 5; i++ {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("i%d", i), i, fmt.Sprintf("item %d body", i), int64(i))
	}

	got, _ := s.SearchThreadMessages("release", 100)
	titleHitsForT1 := 0
	for _, h := range got {
		if h.ThreadID == "t1" && h.MatchType == "title" {
			titleHitsForT1++
		}
	}
	if titleHitsForT1 != 1 {
		t.Errorf("expected exactly one title hit for t1, got %d", titleHitsForT1)
	}
}

// ---- Item-summary matches ----

func TestSearchThreadMessages_MatchOnItemSummary(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "unrelated thread")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "command failed with ECONNREFUSED", 100)

	got, err := s.SearchThreadMessages("econnrefused", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one hit, got %+v", got)
	}
	h := got[0]
	if h.ThreadID != "t1" || h.ItemID != "i1" {
		t.Errorf("wrong hit: %+v", h)
	}
	if h.MatchType != "item" {
		t.Errorf("matchType: got %q, want item", h.MatchType)
	}
	if h.Summary == "" {
		t.Errorf("summary should be populated for item hit")
	}
}

func TestSearchThreadMessages_ItemHitsOrderedByCreatedAtDesc(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	mustInsertItemForSearch(t, s, "t1", "old", 0, "find me older", 100)
	mustInsertItemForSearch(t, s, "t1", "mid", 1, "find me middle", 200)
	mustInsertItemForSearch(t, s, "t1", "new", 2, "find me newest", 300)

	got, _ := s.SearchThreadMessages("find me", 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(got))
	}
	if got[0].ItemID != "new" || got[1].ItemID != "mid" || got[2].ItemID != "old" {
		t.Errorf("expected newest-first ordering, got %v %v %v",
			got[0].ItemID, got[1].ItemID, got[2].ItemID)
	}
}

// ---- Case-insensitivity ----

// Thread find's candidates hold every row the find bar counts a match in:
// the shared vectors (frontend/src/test/fixtures/findVectors.json, read by
// findMatch.test.ts and stringsx) find a summary whenever they count a
// match. The cross-thread search keeps SQLite's ASCII LOWER and LIKE.
func TestSearchThreadItemsMatchesTheSharedFindVectors(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/src/test/fixtures/findVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Count []struct {
			Text  string `json:"text"`
			Query string `json:"query"`
			Count int    `json:"count"`
		} `json:"count"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil || len(vectors.Count) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	s := newTestStore(t)
	for i, v := range vectors.Count {
		if strings.TrimSpace(v.Query) == "" {
			continue
		}
		id := fmt.Sprintf("v%d", i)
		mustCreateThreadForSearch(t, s, id, "vectors")
		mustInsertItemForSearch(t, s, id, id+"-item", 0, v.Text, 100)
		answer, err := s.SearchThreadItems(context.Background(), id, v.Query, TimelineSelection{}, 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		if v.Count > 0 && len(answer.Items) == 0 {
			t.Errorf("thread find of %q in %q missed a row the bar counts", v.Query, v.Text)
		}
	}
	// The cross-thread search keeps its own rule: ASCII letters fold on
	// both sides, every other character matches exactly
	// (TestSearchThreadMessagesMatchesTheSharedSearchVectors).
	mustCreateThreadForSearch(t, s, "accent", "HAUTE ÉCOLE")
	hits, err := s.SearchThreadMessages("haute école", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ThreadID == "accent" {
			t.Errorf("cross-thread search folded É in the text: %+v", h)
		}
	}
	if hits, err := s.SearchThreadMessages("HAUTE", 10); err != nil || len(hits) != 1 || hits[0].ThreadID != "accent" {
		t.Errorf("cross-thread search of HAUTE = %+v, %v", hits, err)
	}
}

func TestSearchThreadMessages_CaseInsensitiveMatch(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "LOUD TITLE")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "quiet body text", 100)

	cases := []string{"loud", "LOUD", "LoUd", "title", "TITLE"}
	for _, q := range cases {
		got, _ := s.SearchThreadMessages(q, 10)
		if len(got) == 0 {
			t.Errorf("case-insensitive match failed for query %q", q)
		}
	}
}

// ---- LIKE-wildcard safety ----

// Each LIKE pattern character in a query matches only itself: the decoy
// beside each title is what a wildcard or a lost escape would also find.
func TestSearchThreadMessages_LiteralPatternCharacters(t *testing.T) {
	s := newTestStore(t)
	for id, title := range map[string]string{
		"underscore": "file_name", "underscore-decoy": "filename",
		"percent": "100% coverage", "percent-decoy": "coverage",
		"backslash": `C:\path\to\file`,
	} {
		mustCreateThreadForSearch(t, s, id, title)
	}

	for _, tc := range []struct{ name, query string }{
		{"underscore", "file_name"},
		{"percent", "100%"},
		{"backslash", `C:\path`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := s.SearchThreadMessages(tc.query, 10)
			if len(got) != 1 || got[0].ThreadID != tc.name {
				t.Errorf("expected only %s for literal query %q, got %+v", tc.name, tc.query, got)
			}
		})
	}
}

// ---- Limit ----

func TestSearchThreadMessages_LimitCapsResults(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	for i := 0; i < 12; i++ {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("i%d", i), i, "same match body", int64(100+i))
	}

	for _, tc := range []struct{ limit, want int }{{5, 5}, {0, 12}} {
		t.Run(fmt.Sprintf("limit %d", tc.limit), func(t *testing.T) {
			if got, _ := s.SearchThreadMessages("match", tc.limit); len(got) != tc.want {
				t.Errorf("expected %d hits for limit %d, got %d", tc.want, tc.limit, len(got))
			}
		})
	}
}

// ---- Cross-thread ordering ----

func TestSearchThreadMessages_AcrossMultipleThreadsAndTypes(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "Quick task")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "Ran the slow query plan to tune", 100)

	mustCreateThreadForSearch(t, s, "t2", "Slow query deep dive")
	mustInsertItemForSearch(t, s, "t2", "i2", 0, "notes on disk io", 200)

	got, _ := s.SearchThreadMessages("slow query", 10)
	// t2 title matches the phrase → must appear first.
	if len(got) < 2 {
		t.Fatalf("expected ≥2 hits, got %+v", got)
	}
	if got[0].ThreadID != "t2" || got[0].MatchType != "title" {
		t.Errorf("expected t2 title hit first, got %+v", got[0])
	}
}

// TestSearchThreadMessages_TitleFloodDoesNotHideItemHits is the regression
// guard for the "one busy titled thread hides every message match" bug. A
// thread whose TITLE matches and which has more items than the limit used to
// fan out (LEFT JOIN) into limit-many 'title' rows that filled the whole
// window before the Go-side dedup ran — so item-summary matches in OTHER
// threads never surfaced. The fix limits title hits and item hits separately,
// so an item match cannot be starved by a title flood.
func TestSearchThreadMessages_TitleFloodDoesNotHideItemHits(t *testing.T) {
	s := newTestStore(t)
	// Title-matching thread with MORE items than the search limit. None of its
	// items contain the term in their summary — only the title matches.
	mustCreateThreadForSearch(t, s, "flooder", "All about seconds and timing")
	for i := 0; i < 60; i++ {
		mustInsertItemForSearch(t, s, "flooder", fmt.Sprintf("f%d", i), i, fmt.Sprintf("ordinary line %d", i), int64(i))
	}
	// A different thread whose ITEM summary matches the term.
	mustCreateThreadForSearch(t, s, "other", "Unrelated title")
	mustInsertItemForSearch(t, s, "other", "hit", 0, "it took thirty seconds to finish", 1000)

	got, err := s.SearchThreadMessages("seconds", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	var sawItemHit bool
	for _, h := range got {
		if h.MatchType == "item" && h.ThreadID == "other" && h.ItemID == "hit" {
			sawItemHit = true
		}
	}
	if !sawItemHit {
		t.Errorf("item-summary match in 'other' was hidden by the title flood; got %d hits: %+v", len(got), got)
	}
}

// TestSearchThreadMessages_TitleFloodAcrossThreadsStillShowsItemHit guards the
// cousin of the flood bug, one level up: a query that matches MANY distinct
// thread TITLES must still surface a message-body match instead of filling the
// whole window with titles. Pre-fix the merge truncated title-first to the
// limit, so 50+ title matches dropped every item hit — re-creating exactly the
// "bodies never surface" complaint the fix set out to cure.
func TestSearchThreadMessages_TitleFloodAcrossThreadsStillShowsItemHit(t *testing.T) {
	s := newTestStore(t)
	// 60 distinct threads whose TITLE contains the term — more than the limit.
	for i := 0; i < 60; i++ {
		mustCreateThreadForSearch(t, s, fmt.Sprintf("title%d", i), fmt.Sprintf("seconds matter %d", i))
	}
	// One thread whose only match is in an item summary.
	mustCreateThreadForSearch(t, s, "body", "Unrelated")
	mustInsertItemForSearch(t, s, "body", "hit", 0, "it took thirty seconds", 1000)

	got, err := s.SearchThreadMessages("seconds", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("expected the result capped at 50, got %d", len(got))
	}
	var sawItemHit bool
	for _, h := range got {
		if h.MatchType == "item" && h.ItemID == "hit" {
			sawItemHit = true
		}
	}
	if !sawItemHit {
		t.Errorf("body match starved by 60 title matches; got %d hits", len(got))
	}
}

// ---- Merge policy (mergeTitleFirst) ----

// TestMergeTitleFirst pins the title/item reservation math directly, without a
// DB: titles lead, but item hits get up to half the window so a title flood
// can't hide them, and whichever kind is sparse cedes its slack to the other.
func TestMergeTitleFirst(t *testing.T) {
	mk := func(kind string, n int) []ThreadMessageHit {
		out := make([]ThreadMessageHit, n)
		for i := range out {
			out[i] = ThreadMessageHit{ThreadID: fmt.Sprintf("%s%d", kind, i), MatchType: kind}
		}
		return out
	}
	countKind := func(hits []ThreadMessageHit, kind string) int {
		n := 0
		for _, h := range hits {
			if h.MatchType == kind {
				n++
			}
		}
		return n
	}

	t.Run("both fit under the limit: all returned, titles first", func(t *testing.T) {
		got := mergeTitleFirst(mk("title", 2), mk("item", 3), 50)
		if len(got) != 5 {
			t.Fatalf("want 5, got %d", len(got))
		}
		// Two titles lead, then the three items.
		if got[0].MatchType != "title" || got[1].MatchType != "title" || got[2].MatchType != "item" {
			t.Errorf("titles should lead, then items: %+v", got)
		}
	})

	t.Run("title flood: half the window is reserved for items", func(t *testing.T) {
		got := mergeTitleFirst(mk("title", 60), mk("item", 30), 50)
		if len(got) != 50 {
			t.Fatalf("want 50, got %d", len(got))
		}
		if titles, items := countKind(got, "title"), countKind(got, "item"); titles != 25 || items != 25 {
			t.Errorf("want 25 titles + 25 items, got %d + %d", titles, items)
		}
	})

	t.Run("sparse items: titles reclaim the unused reserve", func(t *testing.T) {
		got := mergeTitleFirst(mk("title", 60), mk("item", 5), 50)
		if titles, items := countKind(got, "title"), countKind(got, "item"); titles != 45 || items != 5 {
			t.Errorf("want 45 titles + 5 items, got %d + %d", titles, items)
		}
	})

	t.Run("sparse titles: items fill the remainder", func(t *testing.T) {
		got := mergeTitleFirst(mk("title", 3), mk("item", 100), 50)
		if titles, items := countKind(got, "title"), countKind(got, "item"); titles != 3 || items != 47 {
			t.Errorf("want 3 titles + 47 items, got %d + %d", titles, items)
		}
	})

	t.Run("non-positive limit returns everything", func(t *testing.T) {
		got := mergeTitleFirst(mk("title", 60), mk("item", 60), 0)
		if len(got) != 120 {
			t.Errorf("want 120 (unbounded), got %d", len(got))
		}
	})
}

// ---- In-thread find (SearchThreadItems) ----

func TestSearchThreadItems_EmptyQueryAnswersNoRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "alpha")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "hello seconds", 100)

	got, err := s.SearchThreadItems(context.Background(), "t1", "   ", TimelineSelection{}, 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got.Items) != 0 || got.Items == nil || got.Changed == nil {
		t.Errorf("whitespace query answered %+v, want empty non-nil lists", got)
	}
}

func TestSearchThreadItems_ScopedToOneThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "thread one")
	mustInsertItemForSearch(t, s, "t1", "a1", 0, "it took thirty seconds", 100)
	mustCreateThreadForSearch(t, s, "t2", "thread two")
	mustInsertItemForSearch(t, s, "t2", "b1", 0, "also thirty seconds here", 100)

	got, err := s.SearchThreadItems(context.Background(), "t1", "seconds", TimelineSelection{}, 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ThreadID != "t1" || got.Items[0].ID != "a1" {
		t.Fatalf("expected only t1/a1, got %+v", got.Items)
	}
	if got.Items[0].Summary != "it took thirty seconds" || got.Items[0].Kind != "assistant_text" {
		t.Errorf("the candidate is not the row as a page serves it: %+v", got.Items[0])
	}
}

// In-thread find ignores the thread's own title — it searches message text
// only, so a term that appears solely in the title yields nothing.
func TestSearchThreadItems_DoesNotMatchTitle(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "All about seconds")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "no temporal words here", 100)

	got, _ := s.SearchThreadItems(context.Background(), "t1", "seconds", TimelineSelection{}, 10, nil)
	if len(got.Items) != 0 {
		t.Errorf("expected no candidates (term only in title), got %+v", got.Items)
	}
}

// Results step top-to-bottom in document order (turn_index, then item_index),
// not by recency — so "next match" walks down the transcript.
func TestSearchThreadItems_DocumentOrder(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	// Insert out of document order, with created_at intentionally inverted
	// relative to turn order to prove ordering keys off turn/item index.
	mustInsertItemForSearch(t, s, "t1", "third", 2, "find me C", 100)
	mustInsertItemForSearch(t, s, "t1", "first", 0, "find me A", 300)
	mustInsertItemForSearch(t, s, "t1", "second", 1, "find me B", 200)

	got, _ := s.SearchThreadItems(context.Background(), "t1", "find me", TimelineSelection{}, 10, nil)
	if ids := findHitIDs(got); ids != "[first second third]" {
		t.Errorf("expected document order first,second,third; got %s", ids)
	}
}

// A full answer stops at its limit and says it was capped only when a
// candidate lies past it.
func TestSearchThreadItems_LimitCapsAndZeroReturnsAll(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	for i := 0; i < 9; i++ {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("i%d", i), i, "same match body", int64(100+i))
	}

	capped, _ := s.SearchThreadItems(context.Background(), "t1", "match", TimelineSelection{}, 4, nil)
	if len(capped.Items) != 4 || !capped.Capped {
		t.Errorf("limit 4: %d candidates, capped %v; want 4, true", len(capped.Items), capped.Capped)
	}
	exact, _ := s.SearchThreadItems(context.Background(), "t1", "match", TimelineSelection{}, 9, nil)
	if len(exact.Items) != 9 || exact.Capped {
		t.Errorf("limit 9: %d candidates, capped %v; want 9, false", len(exact.Items), exact.Capped)
	}
	all, _ := s.SearchThreadItems(context.Background(), "t1", "match", TimelineSelection{}, 0, nil)
	if len(all.Items) != 9 || all.Capped {
		t.Errorf("zero limit: %d candidates, capped %v; want 9, false", len(all.Items), all.Capped)
	}
}

// The secondary ORDER BY key (item_index) breaks ties within a turn, so two
// items in the same turn step in item_index order — not insertion or recency
// order. mustInsertItemForSearch hardcodes item_index 0, so insert directly.
func TestSearchThreadItems_ItemIndexBreaksTurnTies(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	// Same turn, inserted out of item_index order with inverted created_at to
	// prove neither insertion nor recency decides the tie.
	if err := s.InsertItem(Item{
		ID: "second", ThreadID: "t1", TurnIndex: 0, ItemIndex: 1,
		Kind: "assistant_text", Role: "assistant", Summary: "find me second", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "first", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "find me first", CreatedAt: 200,
	}); err != nil {
		t.Fatalf("insert first: %v", err)
	}

	got, _ := s.SearchThreadItems(context.Background(), "t1", "find me", TimelineSelection{}, 10, nil)
	if ids := findHitIDs(got); ids != "[first second]" {
		t.Errorf("expected item_index order first,second; got %s", ids)
	}
}

// The filter matches a query's letters and digits, never its punctuation
// as a pattern: a % is no wildcard, and both rows whose letters and digits
// read "100" stay candidates for the bar to decide between.
func TestSearchThreadItems_PunctuationIsNoPattern(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	mustInsertItemForSearch(t, s, "t1", "pct", 0, "build is 100% done", 100)
	mustInsertItemForSearch(t, s, "t1", "nopct", 1, "build is 100 done", 200)
	mustInsertItemForSearch(t, s, "t1", "other", 2, "build is 10 done", 300)

	got, _ := s.SearchThreadItems(context.Background(), "t1", "100%", TimelineSelection{}, 10, nil)
	if ids := findHitIDs(got); ids != "[pct nopct]" {
		t.Errorf("100%% candidates = %s, want [pct nopct]", ids)
	}
	// A query of punctuation alone narrows nothing: every row is a
	// candidate and the limit decides, which the answer reports.
	got, _ = s.SearchThreadItems(context.Background(), "t1", "%", TimelineSelection{}, 2, nil)
	if ids := findHitIDs(got); ids != "[pct nopct]" || !got.Capped {
		t.Errorf("%% candidates = %s capped %v, want [pct nopct] capped", ids, got.Capped)
	}
}

// ---- Adversarial ----

func TestSearchThreadMessages_UnicodeQuery(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "日本語 notes")
	mustInsertItemForSearch(t, s, "t1", "i1", 0, "hello", 100)

	got, _ := s.SearchThreadMessages("日本語", 10)
	if len(got) != 1 {
		t.Errorf("expected 1 hit for unicode query, got %+v", got)
	}
}

func TestSearchThreadMessages_HugeQueryDoesNotPanic(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	// A ~50 KB query string. SQLite caps LIKE pattern complexity, so this
	// may legitimately return an error — but it must never panic the store.
	// Either outcome is acceptable: the test is about containment, not a
	// successful search result.
	bigQuery := "a"
	for len(bigQuery) < 50_000 {
		bigQuery += bigQuery
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("search panicked on huge query: %v", r)
		}
	}()
	_, _ = s.SearchThreadMessages(bigQuery, 5)
}

func TestSearchThreadMessages_ProviderFieldPropagates(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	// Mix Claude and Codex threads to confirm the provider field is populated
	// per-hit (used by the UI to render the provider badge).
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: "tc", Title: "claude side", Provider: "claude",
		WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create claude: %v", err)
	}
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: "tx", Title: "codex side", Provider: "codex",
		WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create codex: %v", err)
	}

	got, _ := s.SearchThreadMessages("side", 10)
	providers := map[string]string{}
	for _, h := range got {
		providers[h.ThreadID] = h.Provider
	}
	if providers["tc"] != "claude" {
		t.Errorf("claude provider missing: %+v", providers)
	}
	if providers["tx"] != "codex" {
		t.Errorf("codex provider missing: %+v", providers)
	}
}

// The cross-thread search's match rule, held to the vectors the search
// modal's highlighter reads (frontend/src/test/fixtures/searchVectors.json,
// highlight.test.ts): a title is found exactly when the vector has a range,
// each range alone is found, and the title with its ranges blanked out is
// not, so the highlighter paints every match the SQL made and no other.
func TestSearchThreadMessagesMatchesTheSharedSearchVectors(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/src/test/fixtures/searchVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Ranges []struct {
			Text   string   `json:"text"`
			Query  string   `json:"query"`
			Ranges [][2]int `json:"ranges"`
		} `json:"ranges"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil || len(vectors.Ranges) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	s := newTestStore(t)
	next := 0
	// found reports whether a thread titled title is a hit for query.
	found := func(title, query string) bool {
		t.Helper()
		next++
		id := fmt.Sprintf("sv%d", next)
		mustCreateThreadForSearch(t, s, id, title)
		hits, err := s.SearchThreadMessages(query, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.ThreadID == id {
				return true
			}
		}
		return false
	}
	for _, v := range vectors.Ranges {
		units := utf16.Encode([]rune(v.Text))
		if got := found(v.Text, v.Query); got != (len(v.Ranges) > 0) {
			t.Errorf("search of %q in %q found %v, vector has ranges %v", v.Query, v.Text, got, v.Ranges)
		}
		blanked := append([]uint16(nil), units...)
		for _, r := range v.Ranges {
			if part := string(utf16.Decode(units[r[0]:r[1]])); !found(part, v.Query) {
				t.Errorf("search of %q: range %v (%q) of %q is not a match", v.Query, r, part, v.Text)
			}
			for i := r[0]; i < r[1]; i++ {
				blanked[i] = 1
			}
		}
		if len(v.Ranges) > 0 {
			if rest := string(utf16.Decode(blanked)); found(rest, v.Query) {
				t.Errorf("search of %q in %q matches outside ranges %v (%q)", v.Query, v.Text, v.Ranges, rest)
			}
		}
	}
}
