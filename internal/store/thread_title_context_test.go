package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// seedTitleContextItem inserts one top-level timeline row for the
// ThreadTitleContextItems tests.
func seedTitleContextItem(t *testing.T, s *Store, threadID string, turn, index int, kind, role, summary, parentID string) {
	t.Helper()
	seedTitleContextItemWithMeta(t, s, threadID, turn, index, kind, role, summary, parentID, "")
}

func seedTitleContextItemWithMeta(t *testing.T, s *Store, threadID string, turn, index int, kind, role, summary, parentID, meta string) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        summary + "-id",
		ThreadID:  threadID,
		TurnIndex: turn,
		ItemIndex: index,
		Kind:      kind,
		Role:      role,
		Status:    "completed",
		Summary:   summary,
		ParentID:  parentID,
		Meta:      meta,
		CreatedAt: 1,
		UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert item %s: %v", summary, err)
	}
}

func titleContextSummaries(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Summary)
	}
	return out
}

func assertSummaries(t *testing.T, got []Item, want ...string) {
	t.Helper()
	summaries := titleContextSummaries(got)
	if len(summaries) != len(want) {
		t.Fatalf("summaries = %v, want %v", summaries, want)
	}
	for i := range want {
		if summaries[i] != want[i] {
			t.Fatalf("summaries = %v, want %v", summaries, want)
		}
	}
}

// TestThreadTitleContextItemsOrdersOldestFirstAndFiltersKinds pins the
// projection: conversation rows only, top-level only, ascending.
func TestThreadTitleContextItemsOrdersOldestFirstAndFiltersKinds(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-ctx")

	seedTitleContextItem(t, s, "t-title-ctx", 0, 0, "user_text", "user", "first ask", "")
	seedTitleContextItem(t, s, "t-title-ctx", 0, 1, "tool_call", "assistant", "ran a tool", "")
	seedTitleContextItem(t, s, "t-title-ctx", 0, 2, "assistant_text", "assistant", "first answer", "")
	seedTitleContextItem(t, s, "t-title-ctx", 1, 0, "assistant_text", "assistant", "subagent chatter", "user:0")
	seedTitleContextItem(t, s, "t-title-ctx", 1, 1, "user_text", "user", "second ask", "")

	got, dropped, err := s.ThreadTitleContextItems("t-title-ctx", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if dropped {
		t.Fatal("dropped = true with a window larger than the thread")
	}
	assertSummaries(t, got, "first ask", "first answer", "second ask")
	if got[0].PayloadKind != "" {
		t.Fatalf("PayloadKind = %q, want empty (payload join skipped)", got[0].PayloadKind)
	}
}

// TestThreadTitleContextItemsKeepsNewestWindowPlusFirstUser is the
// windowing contract: the limit takes the newest rows, and the thread's
// first user message is re-attached at the front.
func TestThreadTitleContextItemsKeepsNewestWindowPlusFirstUser(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-window")

	seedTitleContextItem(t, s, "t-title-window", 0, 0, "user_text", "user", "original ask", "")
	seedTitleContextItem(t, s, "t-title-window", 1, 0, "assistant_text", "assistant", "middle answer", "")
	seedTitleContextItem(t, s, "t-title-window", 2, 0, "user_text", "user", "follow up", "")
	seedTitleContextItem(t, s, "t-title-window", 3, 0, "assistant_text", "assistant", "latest answer", "")

	got, dropped, err := s.ThreadTitleContextItems("t-title-window", 2)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if !dropped {
		t.Fatal("dropped = false, want true (two rows fell outside the window)")
	}
	assertSummaries(t, got, "original ask", "follow up", "latest answer")
}

// TestThreadTitleContextItemsReportsDroppedRows is the formatter's
// signal: the window excluded matching rows even though the survivors
// are short enough to fit any character budget whole.
func TestThreadTitleContextItemsReportsDroppedRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-dropped")

	for i := range 6 {
		seedTitleContextItem(t, s, "t-title-dropped", i, 0, "assistant_text", "assistant", "answer "+string(rune('a'+i)), "")
	}

	got, dropped, err := s.ThreadTitleContextItems("t-title-dropped", 2)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if !dropped {
		t.Fatal("dropped = false, want true (4 of 6 rows excluded)")
	}
	assertSummaries(t, got, "answer e", "answer f")

	// Exactly at the limit is NOT a drop: the extra probe row must not be
	// mistaken for one.
	got, dropped, err = s.ThreadTitleContextItems("t-title-dropped", 6)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems(exact): %v", err)
	}
	if dropped {
		t.Fatal("dropped = true when the window holds every row")
	}
	if len(got) != 6 {
		t.Fatalf("rows = %d, want 6", len(got))
	}
}

// TestThreadTitleContextItemsScopesToOneThread: a second thread's rows
// must never leak into the first's context, in either statement.
func TestThreadTitleContextItemsScopesToOneThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-mine")
	mustCreateThread(t, s, "t-title-theirs")

	seedTitleContextItem(t, s, "t-title-mine", 0, 0, "user_text", "user", "my ask", "")
	seedTitleContextItem(t, s, "t-title-mine", 1, 0, "assistant_text", "assistant", "my answer", "")
	seedTitleContextItem(t, s, "t-title-theirs", 0, 0, "user_text", "user", "their ask", "")
	seedTitleContextItem(t, s, "t-title-theirs", 1, 0, "assistant_text", "assistant", "their answer", "")

	got, dropped, err := s.ThreadTitleContextItems("t-title-mine", 1)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if !dropped {
		t.Fatal("dropped = false, want true")
	}
	// The prepended earliest-user row is the OTHER thread's most likely
	// leak point, since it is a second statement.
	assertSummaries(t, got, "my ask", "my answer")
}

// TestThreadTitleContextItemsBoundsSummaryBytes: an oversized summary
// comes back sliced to the span the formatter can reach, keeping its
// TAIL (the window's rows) and staying valid UTF-8.
func TestThreadTitleContextItemsBoundsSummaryBytes(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-huge")

	// Multibyte on purpose: SQLite's substr counts characters, so the
	// slice is a character count over a byte-budgeted formatter.
	huge := "opening marker " + strings.Repeat("é", 60_000) + " closing marker"
	seedTitleContextItem(t, s, "t-title-huge", 0, 0, "user_text", "user", "the original ask", "")
	seedTitleContextItem(t, s, "t-title-huge", 1, 0, "assistant_text", "assistant", huge, "")

	got, _, err := s.ThreadTitleContextItems("t-title-huge", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if len(got) != 2 || got[0].Summary != "the original ask" {
		t.Fatalf("rows = %v, want the ask followed by the bounded answer", titleContextSummaries(got))
	}

	bounded := got[1].Summary
	if len(bounded) >= len(huge) {
		t.Fatalf("summary = %d bytes, want bounded below the stored %d", len(bounded), len(huge))
	}
	if utf8.RuneCountInString(bounded) != threadTitleContextSummaryTail {
		t.Fatalf("summary = %d runes, want the %d-character tail", utf8.RuneCountInString(bounded), threadTitleContextSummaryTail)
	}
	if !utf8.ValidString(bounded) {
		t.Fatal("bounded summary is not valid UTF-8")
	}
	if !strings.HasSuffix(bounded, " closing marker") {
		t.Fatal("window rows must keep their TAIL — the formatter windows newest-first")
	}
}

// TestThreadTitleContextItemsEarliestUserKeepsItsHead is the other half:
// the re-attached first user row is capped from the FRONT, because the
// pin keeps a message's prefix.
func TestThreadTitleContextItemsEarliestUserKeepsItsHead(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-head")

	huge := "opening marker " + strings.Repeat("x", 60_000) + " closing marker"
	seedTitleContextItem(t, s, "t-title-head", 0, 0, "user_text", "user", huge, "")
	seedTitleContextItem(t, s, "t-title-head", 1, 0, "assistant_text", "assistant", "an answer", "")
	seedTitleContextItem(t, s, "t-title-head", 2, 0, "assistant_text", "assistant", "the latest answer", "")

	got, dropped, err := s.ThreadTitleContextItems("t-title-head", 1)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if !dropped {
		t.Fatal("dropped = false, want true")
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want the window plus the prepended first user row", len(got))
	}
	pinned := got[0].Summary
	if len(pinned) != threadTitleContextSummaryHead {
		t.Fatalf("pinned summary = %d bytes, want the %d-character head", len(pinned), threadTitleContextSummaryHead)
	}
	if !strings.HasPrefix(pinned, "opening marker ") {
		t.Fatalf("pinned summary lost its head: %q", pinned[:40])
	}
}

// TestThreadTitleContextItemsReadsMetaForUserRowsOnly: attachment names
// are the one thing read out of meta, and only user rows carry them —
// an assistant row's meta can hold large derived blobs.
func TestThreadTitleContextItemsReadsMetaForUserRowsOnly(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-meta")

	seedTitleContextItemWithMeta(t, s, "t-title-meta", 0, 0, "user_text", "user", "an ask", "", `{"attachments":[]}`)
	seedTitleContextItemWithMeta(t, s, "t-title-meta", 0, 1, "assistant_text", "assistant", "an answer", "", `{"codeSpans":[1,2,3]}`)

	got, _, err := s.ThreadTitleContextItems("t-title-meta", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	if got[0].Meta != `{"attachments":[]}` {
		t.Fatalf("user meta = %q, want it carried", got[0].Meta)
	}
	if got[1].Meta != "" {
		t.Fatalf("assistant meta = %q, want it dropped", got[1].Meta)
	}
}

// TestThreadTitleContextItemsDoesNotDuplicateFirstUser covers the
// already-in-window case: the extra read must not append a second copy.
func TestThreadTitleContextItemsDoesNotDuplicateFirstUser(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-nodup")

	seedTitleContextItem(t, s, "t-title-nodup", 0, 0, "user_text", "user", "only ask", "")
	seedTitleContextItem(t, s, "t-title-nodup", 0, 1, "assistant_text", "assistant", "only answer", "")

	got, _, err := s.ThreadTitleContextItems("t-title-nodup", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	assertSummaries(t, got, "only ask", "only answer")
}

// TestThreadTitleContextItemsWindowStartingBeforeFirstUser guards the
// position compare: an assistant row older than the first user message
// is inside the window, so the first user row must not be re-prepended
// out of order.
func TestThreadTitleContextItemsWindowStartingBeforeFirstUser(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-assistant-first")

	seedTitleContextItem(t, s, "t-title-assistant-first", 0, 0, "assistant_text", "assistant", "opening note", "")
	seedTitleContextItem(t, s, "t-title-assistant-first", 0, 1, "user_text", "user", "the ask", "")

	got, _, err := s.ThreadTitleContextItems("t-title-assistant-first", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems: %v", err)
	}
	assertSummaries(t, got, "opening note", "the ask")
}

func TestThreadTitleContextItemsEmptyAndNonPositiveLimit(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-title-empty")

	got, dropped, err := s.ThreadTitleContextItems("t-title-empty", 200)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems(empty thread): %v", err)
	}
	if len(got) != 0 || dropped {
		t.Fatalf("empty thread returned %d rows (dropped=%v)", len(got), dropped)
	}

	seedTitleContextItem(t, s, "t-title-empty", 0, 0, "user_text", "user", "an ask", "")
	got, dropped, err = s.ThreadTitleContextItems("t-title-empty", 0)
	if err != nil {
		t.Fatalf("ThreadTitleContextItems(limit 0): %v", err)
	}
	if len(got) != 0 || dropped {
		t.Fatalf("limit 0 returned %d rows (dropped=%v)", len(got), dropped)
	}
}
