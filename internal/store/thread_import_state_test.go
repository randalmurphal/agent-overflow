package store

import (
	"errors"
	"fmt"
	"testing"
)

// importedThread is a thread row as the session importer writes one: the
// provenance column set, and original provider timestamps.
func importedThread(id, provider string) Thread {
	t := makeThread(id, provider)
	t.ImportSource = provider
	t.CreatedAt = 1000
	t.UpdatedAt = 2000
	return t
}

func TestNewStoreHasSessionImportSchema(t *testing.T) {
	s := newTestStore(t)

	var columns int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('threads') WHERE name = 'import_source'`,
	).Scan(&columns); err != nil {
		t.Fatalf("probe threads columns: %v", err)
	}
	if columns != 1 {
		t.Errorf("threads.import_source present = %d, want 1", columns)
	}

	var tables int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'thread_import_state'`,
	).Scan(&tables); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if tables != 1 {
		t.Errorf("thread_import_state present = %d, want 1", tables)
	}
}

func TestThreadImportSourceRoundTrips(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-import-codex", "codex")); err != nil {
		t.Fatalf("create imported thread: %v", err)
	}
	if err := s.CreateThread(makeThread("t-native", "claude")); err != nil {
		t.Fatalf("create native thread: %v", err)
	}

	got, err := s.GetThread("t-import-codex")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.ImportSource != "codex" {
		t.Errorf("GetThread ImportSource = %q, want codex", got.ImportSource)
	}

	native, err := s.GetThread("t-native")
	if err != nil {
		t.Fatalf("get native thread: %v", err)
	}
	if native.ImportSource != "" {
		t.Errorf("native ImportSource = %q, want empty", native.ImportSource)
	}

	threads, err := s.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	sources := make(map[string]string, len(threads))
	for _, thread := range threads {
		sources[thread.ID] = thread.ImportSource
	}
	if sources["t-import-codex"] != "codex" {
		t.Errorf("ListThreads ImportSource = %q, want codex", sources["t-import-codex"])
	}
	if sources["t-native"] != "" {
		t.Errorf("ListThreads native ImportSource = %q, want empty", sources["t-native"])
	}
}

func TestCreateThreadRefusesUnknownImportSource(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-import-bad", "claude")
	thread.ImportSource = "claude-tui"
	if err := s.CreateThread(thread); !errors.Is(err, ErrInvalidImportSource) {
		t.Fatalf("CreateThread(claude-tui) = %v, want ErrInvalidImportSource", err)
	}
}

// Import provenance is write-once. UpdateThread rewrites the whole row from
// a caller-supplied struct, so a caller that never read the column back (or
// zeroed it) must not be able to erase where the thread came from.
func TestUpdateThreadPreservesImportSource(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-import-update", "claude")); err != nil {
		t.Fatalf("create imported thread: %v", err)
	}

	stale := makeThread("t-import-update", "claude")
	stale.Title = "Renamed"
	if err := s.UpdateThread(stale); err != nil {
		t.Fatalf("update thread: %v", err)
	}

	got, err := s.GetThread("t-import-update")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.Title != "Renamed" {
		t.Fatalf("title = %q, want Renamed", got.Title)
	}
	if got.ImportSource != "claude" {
		t.Errorf("ImportSource = %q after UpdateThread, want claude", got.ImportSource)
	}
}

// A fork of an imported thread is a new AO-native thread against a fresh
// session file — it has no import cursor, so it must not advertise the
// refresh affordance either.
func TestBuildForkedThreadDropsImportSource(t *testing.T) {
	fork := BuildForkedThread(importedThread("t-import-source", "claude"))
	if fork.ImportSource != "" {
		t.Errorf("forked ImportSource = %q, want empty", fork.ImportSource)
	}
}

func TestThreadImportStateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-state", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if _, ok, err := s.GetThreadImportState("t-state"); err != nil || ok {
		t.Fatalf("GetThreadImportState before write = ok:%v err:%v", ok, err)
	}

	state := ThreadImportState{
		ThreadID:        "t-state",
		Provider:        "claude",
		SourcePath:      "/home/u/.claude/projects/slug/sess.jsonl",
		SourceSessionID: "sess-a",
		LeafUUID:        "leaf-1",
		LastSourceUUID:  "uuid-9",
		LastTurnIndex:   4,
		LastItemIndex:   41,
		ImportedAt:      1700,
	}
	if err := s.SetThreadImportState(state); err != nil {
		t.Fatalf("set thread import state: %v", err)
	}
	got, ok, err := s.GetThreadImportState("t-state")
	if err != nil || !ok {
		t.Fatalf("GetThreadImportState = ok:%v err:%v", ok, err)
	}
	if got != state {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, state)
	}

	// A refresh is the same row with the cursor advanced.
	state.LastSourceUUID = "uuid-20"
	state.LastTurnIndex = 7
	state.LastItemIndex = 63
	state.RefreshedAt = 1900
	if err := s.SetThreadImportState(state); err != nil {
		t.Fatalf("refresh thread import state: %v", err)
	}
	got, ok, err = s.GetThreadImportState("t-state")
	if err != nil || !ok {
		t.Fatalf("GetThreadImportState after refresh = ok:%v err:%v", ok, err)
	}
	if got != state {
		t.Fatalf("refresh mismatch:\n got %+v\nwant %+v", got, state)
	}

	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM thread_import_state WHERE thread_id = 't-state'`,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("thread_import_state rows = %d, want 1 (refresh must update, not append)", rows)
	}
}

// TestHasItemsAfterCursorComparesThePair is the reason the cursor is two
// columns. item_index restarts at 0 in every turn, so a single index cannot
// separate "the import's last row" from "a row a later live turn wrote" —
// both can be item 2. Only the (turn_index, item_index) pair can.
func TestHasItemsAfterCursorComparesThePair(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-cursor", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// An import that wrote turns 1..2, three rows in each.
	for turn := 1; turn <= 2; turn++ {
		for item := 0; item < 3; item++ {
			if _, err := s.UpsertItem(Item{
				ID:        fmt.Sprintf("i-%d-%d", turn, item),
				ThreadID:  "t-cursor",
				TurnIndex: turn,
				ItemIndex: item,
				Kind:      "user_text",
				Role:      "user",
				Status:    "completed",
				Summary:   "hi",
				CreatedAt: 100,
				UpdatedAt: 100,
			}, nil); err != nil {
				t.Fatalf("upsert item: %v", err)
			}
		}
	}

	cases := []struct {
		name      string
		turn, idx int
		want      bool
	}{
		{"cursor at the last row", 2, 2, false},
		{"cursor past the last row", 2, 9, false},
		{"cursor before the last row of its turn", 2, 1, true},
		// The whole point: item 2 of turn 1 is BEHIND item 0 of turn 2,
		// even though a bare item-index compare (2 > 0) says otherwise.
		{"cursor at the last row of an earlier turn", 1, 2, true},
		{"nothing imported", -1, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.HasItemsAfterCursor("t-cursor", tc.turn, tc.idx)
			if err != nil {
				t.Fatalf("HasItemsAfterCursor: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasItemsAfterCursor(%d, %d) = %v, want %v", tc.turn, tc.idx, got, tc.want)
			}
		})
	}

	if _, err := s.HasItemsAfterCursor("", 0, 0); err == nil {
		t.Error("HasItemsAfterCursor accepted an empty thread id")
	}
}

func TestSetThreadImportStateRefusesBadInput(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-state-bad", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	base := ThreadImportState{
		ThreadID:        "t-state-bad",
		Provider:        "claude",
		SourceSessionID: "sess-b",
		ImportedAt:      1,
	}

	noThread := base
	noThread.ThreadID = ""
	if err := s.SetThreadImportState(noThread); err == nil {
		t.Error("SetThreadImportState accepted an empty thread id")
	}

	badProvider := base
	badProvider.Provider = "gemini"
	if err := s.SetThreadImportState(badProvider); !errors.Is(err, ErrInvalidImportProvider) {
		t.Errorf("SetThreadImportState(gemini) = %v, want ErrInvalidImportProvider", err)
	}

	noSession := base
	noSession.SourceSessionID = ""
	if err := s.SetThreadImportState(noSession); err == nil {
		t.Error("SetThreadImportState accepted an empty source session id")
	}

	unknownThread := base
	unknownThread.ThreadID = "t-state-missing"
	if err := s.SetThreadImportState(unknownThread); err == nil {
		t.Error("SetThreadImportState accepted a row for an unknown thread")
	}
}

func TestListImportedSessionRefsUnionsEverySource(t *testing.T) {
	s := newTestStore(t)

	live := makeThread("t-live", "claude")
	live.SessionRef = "sess-live"
	if err := s.CreateThread(live); err != nil {
		t.Fatalf("create live thread: %v", err)
	}

	// A fork whose session file exists but has never been resumed: it has
	// no session_ref at all, which is exactly the row a session_ref-only
	// dedup check would miss and re-import.
	fork := makeThread("t-fork", "claude")
	fork.PendingForkRef = "sess-fork"
	if err := s.CreateThread(fork); err != nil {
		t.Fatalf("create fork thread: %v", err)
	}

	imported := importedThread("t-imported", "codex")
	if err := s.CreateThread(imported); err != nil {
		t.Fatalf("create imported thread: %v", err)
	}
	if err := s.SetThreadImportState(ThreadImportState{
		ThreadID:        "t-imported",
		Provider:        "codex",
		SourcePath:      "/home/u/.codex/sessions/rollout.jsonl",
		SourceSessionID: "sess-imported",
		ImportedAt:      5,
	}); err != nil {
		t.Fatalf("set thread import state: %v", err)
	}

	// A thread with nothing to contribute must not add an empty key.
	if err := s.CreateThread(makeThread("t-draft", "claude")); err != nil {
		t.Fatalf("create draft thread: %v", err)
	}

	refs, err := s.ListImportedSessionRefs()
	if err != nil {
		t.Fatalf("list imported session refs: %v", err)
	}
	want := map[string]string{
		"sess-live":     "t-live",
		"sess-fork":     "t-fork",
		"sess-imported": "t-imported",
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for ref, threadID := range want {
		if refs[ref] != threadID {
			t.Errorf("refs[%q] = %q, want %q", ref, refs[ref], threadID)
		}
	}
}

// A thread deletion must take its dedup entry with it, or the session it
// came from stays permanently unimportable.
func TestListImportedSessionRefsDropsDeletedThreads(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(importedThread("t-gone", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.SetThreadImportState(ThreadImportState{
		ThreadID:        "t-gone",
		Provider:        "claude",
		SourceSessionID: "sess-gone",
		ImportedAt:      1,
	}); err != nil {
		t.Fatalf("set thread import state: %v", err)
	}
	if err := s.DeleteThread("t-gone"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	refs, err := s.ListImportedSessionRefs()
	if err != nil {
		t.Fatalf("list imported session refs: %v", err)
	}
	if _, found := refs["sess-gone"]; found {
		t.Errorf("refs still carry a deleted thread's session: %v", refs)
	}
}
