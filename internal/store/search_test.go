package store

import (
	"fmt"
	"testing"
	"time"
)

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

	got, err := s.SearchThreadMessages("", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil hits for empty query, got %+v", got)
	}
}

func TestSearchThreadMessages_WhitespaceOnlyReturnsNil(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "alpha")

	got, _ := s.SearchThreadMessages("   \t\n  ", 10)
	if got != nil {
		t.Errorf("expected nil hits for whitespace query, got %+v", got)
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

// ---- Title-first ordering ----

func TestSearchThreadMessages_TitleMatchesSortedBeforeItemMatches(t *testing.T) {
	s := newTestStore(t)
	// t1 has a title match.
	mustCreateThreadForSearch(t, s, "t1", "Release notes draft")
	// t2 has only an item match on the same term.
	mustCreateThreadForSearch(t, s, "t2", "Unrelated")
	mustInsertItemForSearch(t, s, "t2", "i1", 0, "Preparing the release notes for Q4", 100)

	got, _ := s.SearchThreadMessages("release", 10)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 hits, got %+v", got)
	}
	if got[0].MatchType != "title" {
		t.Errorf("first hit should be title match, got %q", got[0].MatchType)
	}
}

// ---- Case-insensitivity ----

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

func TestSearchThreadMessages_LiteralUnderscoreNotAWildcard(t *testing.T) {
	s := newTestStore(t)
	// Must match exactly the underscore; adjacent unrelated text won't.
	mustCreateThreadForSearch(t, s, "t1", "file_name")
	mustCreateThreadForSearch(t, s, "t2", "filename")

	got, _ := s.SearchThreadMessages("file_name", 10)
	if len(got) != 1 || got[0].ThreadID != "t1" {
		t.Errorf("expected only t1 for literal underscore query, got %+v", got)
	}
}

func TestSearchThreadMessages_LiteralPercentNotAWildcard(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "100% coverage")
	mustCreateThreadForSearch(t, s, "t2", "coverage")

	got, _ := s.SearchThreadMessages("100%", 10)
	if len(got) != 1 || got[0].ThreadID != "t1" {
		t.Errorf("expected only t1 for literal %% query, got %+v", got)
	}
}

func TestSearchThreadMessages_LiteralBackslashSurvives(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", `C:\path\to\file`)

	got, _ := s.SearchThreadMessages(`C:\path`, 10)
	if len(got) != 1 || got[0].ThreadID != "t1" {
		t.Errorf("expected t1 for backslash query, got %+v", got)
	}
}

// ---- Limit ----

func TestSearchThreadMessages_LimitCapsResults(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	for i := 0; i < 12; i++ {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("i%d", i), i, "same match body", int64(100+i))
	}

	got, _ := s.SearchThreadMessages("match", 5)
	if len(got) != 5 {
		t.Errorf("expected 5 hits (limit), got %d", len(got))
	}
}

func TestSearchThreadMessages_ZeroLimitReturnsAll(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "x")
	for i := 0; i < 8; i++ {
		mustInsertItemForSearch(t, s, "t1", fmt.Sprintf("i%d", i), i, "same match body", int64(100+i))
	}

	got, _ := s.SearchThreadMessages("match", 0)
	if len(got) != 8 {
		t.Errorf("expected 8 hits for zero-limit, got %d", len(got))
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
