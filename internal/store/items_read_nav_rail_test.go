package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ListThreadUserMessageTicks backs the message-nav rail's baseline: the
// contract is every reader-authored user message oldest first, wire-only
// rows (context injections) and subagent children skipped, other
// threads' rows invisible, and an empty thread answering an empty list.
func TestListThreadUserMessageTicks(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, threadID := range []string{"t-a", "t-b", "t-empty", "t-order"} {
		if err := s.CreateThread(Thread{
			ID: threadID, ProjectID: defaultTestProjectID, Title: threadID, Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}

	rows := []Item{
		// Wire-only injection sorts first but must be skipped.
		{ID: "a-wire", ThreadID: "t-a", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "injected context",
			Meta: `{"wire_only":true}`},
		// A subagent child user row is not top-level.
		{ID: "a-child", ThreadID: "t-a", TurnIndex: 0, ItemIndex: 1,
			Kind: "user_text", Role: "user", Summary: "child prompt",
			ParentID: "a-wire"},
		{ID: "a-first", ThreadID: "t-a", TurnIndex: 0, ItemIndex: 2,
			Kind: "user_text", Role: "user", Summary: "the real first ask"},
		{ID: "a-second", ThreadID: "t-a", TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "follow-up"},
		// Another thread's rows must not leak into t-a's answer.
		{ID: "b-first", ThreadID: "t-b", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "other thread"},
		// Two-column ordering exercised independently: a higher
		// item_index in an earlier turn sorts before a lower item_index
		// in a later turn.
		{ID: "o-late-turn", ThreadID: "t-order", TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "turn 1 item 0"},
		{ID: "o-first", ThreadID: "t-order", TurnIndex: 0, ItemIndex: 5,
			Kind: "user_text", Role: "user", Summary: "turn 0 item 5"},
	}
	for _, it := range rows {
		it.CreatedAt, it.UpdatedAt = now, now
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}

	got, err := s.ListThreadUserMessageTicks("t-a", TimelineSelection{})
	if err != nil {
		t.Fatalf("ticks t-a: %v", err)
	}
	want := []UserMessageTick{
		{ID: "a-first", TurnIndex: 0, ItemIndex: 2},
		{ID: "a-second", TurnIndex: 1, ItemIndex: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d ticks, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tick %d: want %+v, got %+v", i, want[i], got[i])
		}
	}

	empty, err := s.ListThreadUserMessageTicks("t-empty", TimelineSelection{})
	if err != nil {
		t.Fatalf("ticks t-empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty thread must answer an empty list, got %+v", empty)
	}

	ordered, err := s.ListThreadUserMessageTicks("t-order", TimelineSelection{})
	if err != nil {
		t.Fatalf("ticks t-order: %v", err)
	}
	if len(ordered) != 2 || ordered[0].ID != "o-first" || ordered[1].ID != "o-late-turn" {
		t.Fatalf("turn_index must dominate item_index: got %+v", ordered)
	}
}

// ListThreadUserMessageHistory backs composer ArrowUp recall: newest
// first, full uncapped text, the shared reader-authored predicate
// (wire-only injections and subagent children skipped), other threads
// invisible, and the limit trimming from the OLD end.
func TestListThreadUserMessageHistory(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, threadID := range []string{"t-h", "t-other"} {
		if err := s.CreateThread(Thread{
			ID: threadID, ProjectID: defaultTestProjectID, Title: threadID, Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}

	long := strings.Repeat("y", 3000)
	rows := []Item{
		{ID: "h-first", ThreadID: "t-h", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "first ask"},
		{ID: "h-wire", ThreadID: "t-h", TurnIndex: 0, ItemIndex: 1,
			Kind: "user_text", Role: "user", Summary: "injected context",
			Meta: `{"wire_only":true}`},
		{ID: "h-child", ThreadID: "t-h", TurnIndex: 0, ItemIndex: 2,
			Kind: "user_text", Role: "user", Summary: "child prompt",
			ParentID: "h-first"},
		{ID: "h-second", ThreadID: "t-h", TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: long},
		{ID: "other", ThreadID: "t-other", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "other thread"},
	}
	for _, it := range rows {
		it.CreatedAt, it.UpdatedAt = now, now
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}

	got, err := s.ListThreadUserMessageHistory("t-h", 50)
	if err != nil {
		t.Fatalf("history t-h: %v", err)
	}
	want := []UserMessageHistoryEntry{
		{ID: "h-second", TurnIndex: 1, ItemIndex: 0, Summary: long},
		{ID: "h-first", TurnIndex: 0, ItemIndex: 0, Summary: "first ask"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: want %+v, got %+v", i, want[i], got[i])
		}
	}

	// The limit keeps the NEWEST rows — recall walks backwards from now.
	capped, err := s.ListThreadUserMessageHistory("t-h", 1)
	if err != nil {
		t.Fatalf("history t-h limit 1: %v", err)
	}
	if len(capped) != 1 || capped[0].ID != "h-second" {
		t.Fatalf("limit must trim the old end, got %+v", capped)
	}

	empty, err := s.ListThreadUserMessageHistory("t-missing", 50)
	if err != nil {
		t.Fatalf("history t-missing: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown thread must answer an empty list, got %+v", empty)
	}
}

// ThreadTurnPreview pairs a tick's ask with the turn's FINAL top-level
// assistant reply; a wire-only injection mid-turn neither ends the turn
// nor becomes the ask, subagent children are invisible, and a
// non-reader-authored anchor answers found=false. Semantics must match
// the frontend's `turnPreview` walk over loaded items.
func TestThreadTurnPreview(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-p", ProjectID: defaultTestProjectID, Title: "t-p", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	long := strings.Repeat("x", 3000)
	rows := []Item{
		{ID: "u1", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "real ask"},
		{ID: "a1", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "early"},
		// Subagent child assistant text must not become the reply.
		{ID: "child", TurnIndex: 0, ItemIndex: 2, Kind: "assistant_text", Role: "assistant",
			Summary: "sub", ParentID: "a1"},
		// A wire-only injection mid-turn is context, not the next ask.
		{ID: "inj", TurnIndex: 0, ItemIndex: 3, Kind: "user_text", Role: "user",
			Summary: "injected", Meta: `{"wire_only":true}`},
		{ID: "a2", TurnIndex: 0, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", Summary: long},
		{ID: "u2", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "next ask"},
		{ID: "a3", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "after next turn"},
	}
	for _, it := range rows {
		it.ThreadID, it.CreatedAt, it.UpdatedAt = "t-p", now, now
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}

	got, found, err := s.ThreadTurnPreview("t-p", "u1")
	if err != nil {
		t.Fatalf("preview u1: %v", err)
	}
	if !found || got.UserText != "real ask" {
		t.Fatalf("want real ask, got found=%v %+v", found, got)
	}
	// The reply is the FINAL top-level assistant text of u1's turn,
	// rune-capped at the wire with an ellipsis marker.
	if len([]rune(got.AssistantText)) != turnPreviewMaxRunes+1 || !strings.HasSuffix(got.AssistantText, "…") {
		t.Fatalf("want capped final reply, got %d runes ending %q",
			len([]rune(got.AssistantText)), got.AssistantText[len(got.AssistantText)-3:])
	}
	if !strings.HasPrefix(got.AssistantText, "xxx") {
		t.Fatalf("reply must come from a2, got prefix %q", got.AssistantText[:8])
	}

	tail, found, err := s.ThreadTurnPreview("t-p", "u2")
	if err != nil {
		t.Fatalf("preview u2: %v", err)
	}
	if !found || tail.UserText != "next ask" || tail.AssistantText != "after next turn" {
		t.Fatalf("tail turn: got found=%v %+v", found, tail)
	}

	// A wire-only anchor and an unknown id both answer found=false.
	if _, found, err = s.ThreadTurnPreview("t-p", "inj"); err != nil || found {
		t.Fatalf("wire-only anchor must answer found=false, got found=%v err=%v", found, err)
	}
	if _, found, err = s.ThreadTurnPreview("t-p", "missing"); err != nil || found {
		t.Fatalf("unknown anchor must answer found=false, got found=%v err=%v", found, err)
	}
}

func TestThreadTurnPreviewUsesChildTranscript(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("scope-preview", "claude")); err != nil {
		t.Fatal(err)
	}
	seedAnchorItem(t, s, "scope-preview", "root", 0, 0)
	rows := []Item{
		{ID: "prompt", Kind: "user_text", ParentID: "root", Summary: "child ask"},
		{ID: "reply", Kind: "assistant_text", ParentID: "root", Summary: "child answer"},
		{ID: "main", Kind: "assistant_text", Summary: "wrong main answer"},
		{ID: "nested", Kind: "assistant_text", ParentID: "nested-root", Summary: "wrong nested answer"},
		{ID: "injection", Kind: "user_text", ParentID: "root", Summary: "internal context", Meta: `{"wire_only":true}`},
		{ID: "final-reply", Kind: "assistant_text", ParentID: "root", Summary: "child final answer"},
		{ID: "next", Kind: "user_text", ParentID: "root", Summary: "next ask"},
		{ID: "next-reply", Kind: "assistant_text", ParentID: "root", Summary: "next answer"},
	}
	for i, row := range rows {
		row.Role = "assistant"
		if row.Kind == "user_text" {
			row.Role = "user"
		}
		row.Status = "completed"
		row.ThreadID = "scope-preview"
		row.ItemIndex = i + 1
		row.CreatedAt = 1
		if err := insertCarded(s, row); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := s.ThreadTurnPreview("scope-preview", "prompt")
	if err != nil || !found || got.UserText != "child ask" || got.AssistantText != "child final answer" {
		t.Fatalf("preview: %+v found=%v err=%v", got, found, err)
	}
	ticks, err := s.ListThreadUserMessageTicks("scope-preview", TimelineSelection{ScopeRootID: "root"})
	if err != nil || len(ticks) != 2 || ticks[0].ID != "prompt" || ticks[1].ID != "next" {
		t.Fatalf("ticks: %+v %v", ticks, err)
	}
	if _, found, err := s.ThreadTurnPreview("scope-preview", "injection"); err != nil || found {
		t.Fatalf("wire-only preview found=%v err=%v", found, err)
	}
	if _, found, err := s.ThreadTurnPreview("another-thread", "prompt"); err != nil || found {
		t.Fatalf("cross-thread preview found=%v err=%v", found, err)
	}
}

// The turn preview reads the text rows the timeline_items view holds after
// a position, in order, with their summaries and wire-only flags: the walk
// selects them and turnPreviewRowsSQL reads each one's row. It does so on
// the main timeline and in agent transcripts, from every row's position of
// a thread with imported, overridden and local rows and of its pointer
// forks, one cut inside a turn with rows of its own past the cut.
func TestTurnPreviewWalkMatchesTheView(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	walk := func(query string, args ...any) []string {
		t.Helper()
		rows, err := s.db.Query(query, args...)
		if err != nil {
			t.Fatalf("walk: %v\n%s", err, query)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var kind, summary string
			var wireOnly, turnIndex, itemIndex int
			if err := rows.Scan(&kind, &summary, &wireOnly, &turnIndex, &itemIndex); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%d/%d %s %q wire=%d", turnIndex, itemIndex, kind, summary, wireOnly))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		return got
	}
	walked := map[string]int{}
	for _, threadID := range seedTimelineParityForks(t, s) {
		type filter struct {
			name string
			sql  string
			args []any
		}
		filters := []filter{{"main", mainTimelineFilterFor("items."), nil}}
		for _, root := range []string{"imp-child-0", "loc-child-2"} {
			scope, err := s.resolveTimelineScope(s.reader(), threadID, TimelineSelection{ScopeRootID: root})
			if err != nil {
				t.Fatal(err)
			}
			sql, args := scope.filter("items.")
			filters = append(filters, filter{root, sql, args})
		}
		rows, err := s.db.Query(`SELECT turn_index, item_index FROM timeline_items WHERE thread_id = ? ORDER BY turn_index, item_index`, threadID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		positions := []TimelineCursor{{TurnIndex: -1, ItemIndex: -1}}
		for rows.Next() {
			var at TimelineCursor
			if err := rows.Scan(&at.TurnIndex, &at.ItemIndex); err != nil {
				t.Fatal(err)
			}
			positions = append(positions, at)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		for _, f := range filters {
			for _, at := range positions {
				query, args := mustTimelineArms(t, s, threadID, turnPreviewWalk(f.sql, slices.Clone(f.args), at))
				got := walk(turnPreviewRowsSQL(query), args...)
				want := walk(`SELECT items.kind, items.summary,
				        COALESCE(CASE WHEN json_valid(items.meta) THEN json_extract(items.meta, '$.wire_only') END, 0),
				        items.turn_index, items.item_index
				   FROM timeline_items AS items
				  WHERE items.thread_id = ? AND `+f.sql+`
				    AND items.kind IN ('user_text', 'assistant_text')
				    AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))
				  ORDER BY items.turn_index, items.item_index LIMIT `+strconv.Itoa(turnPreviewScanLimit),
					slices.Concat([]any{threadID}, f.args, []any{at.TurnIndex, at.TurnIndex, at.ItemIndex})...)
				if !slices.Equal(got, want) {
					t.Errorf("%s %s walk after %d/%d:\n arms: %q\n view: %q", threadID, f.name, at.TurnIndex, at.ItemIndex, got, want)
				}
				walked[threadID+" "+f.name] += len(got)
			}
		}
	}
	for _, key := range []string{"tl-arms main", "tl-arms-cut main", "tl-arms imp-child-0", "tl-arms-whole loc-child-2", "tl-arms-cut loc-child-2"} {
		if walked[key] == 0 {
			t.Errorf("%s walks no row from any position; the check proves nothing there (%v)", key, walked)
		}
	}
}
