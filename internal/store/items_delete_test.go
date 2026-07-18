package store

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
)

func TestDeleteConversationFromTurnRemovesSelectedAndForwardTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-del", now)
	insertDeleteConversationRows(t, s, "t-del", 5, now)
	if err := s.UpsertTrackedFiles("t-del", 1, []string{"keep.txt"}); err != nil {
		t.Fatalf("track keep: %v", err)
	}
	if err := s.UpsertTrackedFiles("t-del", 3, []string{"drop.txt"}); err != nil {
		t.Fatalf("track drop: %v", err)
	}

	_, deleted, err := s.DeleteConversationFromTurn("t-del", 2)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 6 {
		t.Errorf("deleted count: got %d, want 6", deleted)
	}

	items, err := s.ListItems("t-del")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("remaining items: got %d, want 4", len(items))
	}
	for _, it := range items {
		if it.TurnIndex >= 2 {
			t.Errorf("item %s has turn_index %d, should have been deleted", it.ID, it.TurnIndex)
		}
	}
	assertTurnsRemaining(t, s, "t-del", []int{0, 1})
	tracked, err := s.ListTrackedFiles("t-del")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if len(tracked) != 1 || tracked[0] != "keep.txt" {
		t.Fatalf("tracked files after delete = %v, want [keep.txt]", tracked)
	}
}

func TestDeleteConversationFromTurnScopesToThread(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	for _, id := range []string{"t-a", "t-b"} {
		createDeleteConversationThread(t, s, id, now)
		insertDeleteConversationRows(t, s, id, 3, now)
	}

	if _, _, err := s.DeleteConversationFromTurn("t-a", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}

	aItems, err := s.ListItems("t-a")
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	bItems, err := s.ListItems("t-b")
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(aItems) != 2 {
		t.Errorf("t-a remaining items: got %d, want 2", len(aItems))
	}
	if len(bItems) != 6 {
		t.Errorf("t-b remaining items: got %d, want 6", len(bItems))
	}
	assertTurnsRemaining(t, s, "t-a", []int{0})
	assertTurnsRemaining(t, s, "t-b", []int{0, 1, 2})
}

func TestDeleteConversationFromTurnEmptyIsNoop(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-empty", now)

	_, deleted, err := s.DeleteConversationFromTurn("t-empty", 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted on empty thread: got %d, want 0", deleted)
	}
}

// TestDeleteConversationFromTurnDoesNotBumpThreadUpdatedAt — conversation
// truncation is a structural cleanup, not a meaningful interaction. The
// sidebar timestamp stays anchored at the previous interaction. The
// next user_text persist (or whatever the user does next) bumps via
// Store.MarkThreadActivity.
func TestDeleteConversationFromTurnDoesNotBumpThreadUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli() - 10_000
	createDeleteConversationThread(t, s, "t-touch", base)
	insertDeleteConversationRows(t, s, "t-touch", 1, base)

	before, err := s.GetThread("t-touch")
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if _, _, err := s.DeleteConversationFromTurn("t-touch", 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := s.GetThread("t-touch")
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at unexpectedly moved across truncation: before=%d after=%d", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestDeleteConversationFromItemKeepsSameTurnPrefix — the item-granular
// truncation behind Claude reverts to queued flush messages: same-turn rows
// BEFORE the anchor survive (with their turn row, tracked files, and
// checkpoints); the anchor, everything after it, and fully-emptied turns go.
func TestDeleteConversationFromItemKeepsSameTurnPrefix(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-item", now)
	insertDeleteConversationRows(t, s, "t-item", 5, now)
	if err := s.UpsertTrackedFiles("t-item", 2, []string{"anchor-turn.txt"}); err != nil {
		t.Fatalf("track anchor turn: %v", err)
	}
	if err := s.UpsertTrackedFiles("t-item", 3, []string{"later-turn.txt"}); err != nil {
		t.Fatalf("track later turn: %v", err)
	}
	for _, cp := range []struct {
		userItemID, ref string
		turnIndex       int
	}{
		{"t-item-item-2-0", "refs/ao/keep", 2},
		{"t-item-item-2-1", "refs/ao/anchor", 2},
		{"t-item-item-3-0", "refs/ao/later", 3},
	} {
		if _, err := s.ReplaceCheckpointByUserItemID(Checkpoint{
			ID:            cp.userItemID + "-cp",
			ThreadID:      "t-item",
			UserItemID:    cp.userItemID,
			TurnIndex:     cp.turnIndex,
			RefName:       cp.ref,
			Status:        "ready",
			CapturedAt:    now,
			WorkspacePath: "/tmp",
		}); err != nil {
			t.Fatalf("insert checkpoint %s: %v", cp.userItemID, err)
		}
	}

	refs, err := s.DeleteConversationFromItem("t-item", "t-item-item-2-1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	gotRefs := make([]string, len(refs))
	for i, r := range refs {
		gotRefs[i] = r.RefName
	}
	if fmt.Sprint(gotRefs) != fmt.Sprint([]string{"refs/ao/anchor", "refs/ao/later"}) {
		t.Errorf("deleted refs = %v, want [refs/ao/anchor refs/ao/later]", gotRefs)
	}

	items, err := s.ListItems("t-item")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("remaining items: got %d, want 5 (turns 0-1 plus turn 2's first row)", len(items))
	}
	last := items[len(items)-1]
	if last.ID != "t-item-item-2-0" {
		t.Errorf("last surviving item = %s, want t-item-item-2-0", last.ID)
	}
	assertTurnsRemaining(t, s, "t-item", []int{0, 1, 2})

	tracked, err := s.ListTrackedFiles("t-item")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if fmt.Sprint(tracked) != fmt.Sprint([]string{"anchor-turn.txt"}) {
		t.Errorf("tracked files after delete = %v, want [anchor-turn.txt]", tracked)
	}

	if _, ok, err := s.GetCheckpointByUserItemID("t-item", "t-item-item-2-0"); err != nil || !ok {
		t.Errorf("same-turn prefix checkpoint should survive (ok=%v err=%v)", ok, err)
	}
	if _, ok, _ := s.GetCheckpointByUserItemID("t-item", "t-item-item-2-1"); ok {
		t.Error("anchor checkpoint should be deleted")
	}
	if _, ok, _ := s.GetCheckpointByUserItemID("t-item", "t-item-item-3-0"); ok {
		t.Error("later-turn checkpoint should be deleted")
	}
}

// TestDeleteConversationFromItemCheckpointTurnDrift pins R8-3 (round 8):
// ref collection follows the ITEMS the cut deletes, not the checkpoint's
// cached turn_index. A checkpoint whose cached turn drifted below the
// anchor while its user item sits in a deleted later turn must still
// return its ref (the FK cascade deletes the row either way — missing
// the SELECT leaks the git ref); a checkpoint whose cached turn drifted
// above the anchor while its user item survives must be kept.
func TestDeleteConversationFromItemCheckpointTurnDrift(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-drift", now)
	insertDeleteConversationRows(t, s, "t-drift", 5, now)
	for _, cp := range []struct {
		userItemID, ref string
		turnIndex       int
	}{
		// Item survives (same-turn prefix) but cached turn says later.
		{"t-drift-item-2-0", "refs/ao/stale-high", 9},
		// Item is deleted (later turn) but cached turn says earlier.
		{"t-drift-item-3-0", "refs/ao/stale-low", 0},
	} {
		if _, err := s.ReplaceCheckpointByUserItemID(Checkpoint{
			ID:            cp.userItemID + "-cp",
			ThreadID:      "t-drift",
			UserItemID:    cp.userItemID,
			TurnIndex:     cp.turnIndex,
			RefName:       cp.ref,
			Status:        "ready",
			CapturedAt:    now,
			WorkspacePath: "/tmp",
		}); err != nil {
			t.Fatalf("insert checkpoint %s: %v", cp.userItemID, err)
		}
	}

	refs, err := s.DeleteConversationFromItem("t-drift", "t-drift-item-2-1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	gotRefs := make([]string, len(refs))
	for i, r := range refs {
		gotRefs[i] = r.RefName
	}
	if fmt.Sprint(gotRefs) != fmt.Sprint([]string{"refs/ao/stale-low"}) {
		t.Errorf("deleted refs = %v, want [refs/ao/stale-low]", gotRefs)
	}
	if _, ok, err := s.GetCheckpointByUserItemID("t-drift", "t-drift-item-2-0"); err != nil || !ok {
		t.Errorf("surviving item's checkpoint must survive its stale-high cached turn (ok=%v err=%v)", ok, err)
	}
	if _, ok, _ := s.GetCheckpointByUserItemID("t-drift", "t-drift-item-3-0"); ok {
		t.Error("deleted item's checkpoint row should be gone")
	}
}

// TestDeleteConversationFromItemTurnInitialMatchesTurnGranular — when the
// anchor opens its turn, the item-granular delete degenerates to
// DeleteConversationFromTurn: the emptied anchor turn loses its turns row
// and tracked files too.
func TestDeleteConversationFromItemTurnInitialMatchesTurnGranular(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-init", now)
	insertDeleteConversationRows(t, s, "t-init", 4, now)
	if err := s.UpsertTrackedFiles("t-init", 1, []string{"keep.txt"}); err != nil {
		t.Fatalf("track keep: %v", err)
	}
	if err := s.UpsertTrackedFiles("t-init", 2, []string{"drop.txt"}); err != nil {
		t.Fatalf("track drop: %v", err)
	}

	if _, err := s.DeleteConversationFromItem("t-init", "t-init-item-2-0"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err := s.ListItems("t-init")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("remaining items: got %d, want 4", len(items))
	}
	assertTurnsRemaining(t, s, "t-init", []int{0, 1})
	tracked, err := s.ListTrackedFiles("t-init")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if fmt.Sprint(tracked) != fmt.Sprint([]string{"keep.txt"}) {
		t.Errorf("tracked files after delete = %v, want [keep.txt]", tracked)
	}
}

// TestDeleteConversationFromItemPromotedAnchorKeepsSameTurnTail — an
// interrupt-PROMOTED anchor sits ABOVE its turn's interrupted tail in display
// order but BEFORE it in the provider transcript (the tail's final partials
// persisted after the bump; Claude appends the queued_command attachment
// after all of them). Cutting at the anchor keeps same-turn non-user
// successors, deletes same-turn user successors (later-queued messages) and
// later turns, and leaves the turn row's settle metadata alone — the turn's
// own content is intact.
func TestDeleteConversationFromItemPromotedAnchorKeepsSameTurnTail(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-promo", now)

	promotedMeta, err := itemmeta.MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	rows := []struct {
		turn, idx int
		id, role  string
		meta      string
	}{
		{0, 0, "t-promo-user-0", "user", ""},
		{0, 1, "t-promo-reply-0", "assistant", ""},
		{1, 0, "t-promo-prompt", "user", ""},
		{1, 1, "t-promo-pre", "assistant", ""},
		{1, 2, "t-promo-anchor", "user", promotedMeta},
		{1, 3, "t-promo-queued2", "user", promotedMeta},
		{1, 4, "t-promo-tail1", "assistant", ""},
		{1, 5, "t-promo-tail2", "assistant", ""},
		{2, 0, "t-promo-user-2", "user", ""},
		{2, 1, "t-promo-reply-2", "assistant", ""},
	}
	for turn := 0; turn <= 2; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID:    fmt.Sprintf("t-promo-turn-%d", turn),
			ThreadID:  "t-promo",
			TurnIndex: turn,
			StartedAt: now + int64(turn),
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
	}
	for _, r := range rows {
		if err := s.InsertItem(Item{
			ID:        r.id,
			ThreadID:  "t-promo",
			TurnIndex: r.turn,
			ItemIndex: r.idx,
			Kind:      "user_text",
			Role:      r.role,
			Meta:      r.meta,
			CreatedAt: now,
		}); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	// The interrupted turn settled before the revert; a promoted anchor's
	// cut keeps the turn content, so this metadata must survive untouched.
	if err := s.UpdateTurnCompleted("t-promo-turn-1", now+500, "interrupted", "am-1", `{"in":5}`, ""); err != nil {
		t.Fatalf("settle turn 1: %v", err)
	}
	for _, cp := range []struct {
		userItemID, ref string
		turnIndex       int
	}{
		{"t-promo-prompt", "refs/ao/keep", 1},
		{"t-promo-anchor", "refs/ao/anchor", 1},
		{"t-promo-user-2", "refs/ao/later", 2},
	} {
		if _, err := s.ReplaceCheckpointByUserItemID(Checkpoint{
			ID:            cp.userItemID + "-cp",
			ThreadID:      "t-promo",
			UserItemID:    cp.userItemID,
			TurnIndex:     cp.turnIndex,
			RefName:       cp.ref,
			Status:        "ready",
			CapturedAt:    now,
			WorkspacePath: "/tmp",
		}); err != nil {
			t.Fatalf("insert checkpoint %s: %v", cp.userItemID, err)
		}
	}

	refs, err := s.DeleteConversationFromItem("t-promo", "t-promo-anchor")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	gotRefs := make([]string, len(refs))
	for i, r := range refs {
		gotRefs[i] = r.RefName
	}
	if fmt.Sprint(gotRefs) != fmt.Sprint([]string{"refs/ao/anchor", "refs/ao/later"}) {
		t.Errorf("deleted refs = %v, want [refs/ao/anchor refs/ao/later]", gotRefs)
	}

	items, err := s.ListItems("t-promo")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotIDs []string
	for _, it := range items {
		gotIDs = append(gotIDs, it.ID)
	}
	wantIDs := []string{
		"t-promo-user-0", "t-promo-reply-0",
		"t-promo-prompt", "t-promo-pre", "t-promo-tail1", "t-promo-tail2",
	}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Errorf("surviving items = %v, want %v", gotIDs, wantIDs)
	}
	assertTurnsRemaining(t, s, "t-promo", []int{0, 1})

	turn1, ok, err := s.GetTurnByThreadIndex("t-promo", 1)
	if err != nil || !ok {
		t.Fatalf("get turn 1: ok=%v err=%v", ok, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != now+500 {
		t.Errorf("promoted cut moved turn 1 completed_at: %v, want %d", turn1.CompletedAt, now+500)
	}
	if turn1.AssistantMessageID != "am-1" || turn1.StopReason != "interrupted" || turn1.TokenUsageJSON != `{"in":5}` {
		t.Errorf("promoted cut rewrote turn 1 settle metadata: %+v", turn1)
	}

	if _, ok, err := s.GetCheckpointByUserItemID("t-promo", "t-promo-prompt"); err != nil || !ok {
		t.Errorf("prefix checkpoint should survive (ok=%v err=%v)", ok, err)
	}
	if _, ok, _ := s.GetCheckpointByUserItemID("t-promo", "t-promo-anchor"); ok {
		t.Error("anchor checkpoint should be deleted")
	}
}

// TestDeleteConversationFromItemTrimsAnchorTurnSettle — a plain (non-promoted)
// anchor keeps only its same-turn PREFIX, so the surviving turn row's settle
// metadata described the deleted suffix: completed_at trims back to the last
// surviving row's created_at and the deleted response's assistant_message_id
// clears. Token usage and stop_reason stay — the spend was real and the
// ledger already recorded it.
func TestDeleteConversationFromItemTrimsAnchorTurnSettle(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-trim", base)
	if err := s.InsertTurn(Turn{
		TurnID:    "t-trim-turn-0",
		ThreadID:  "t-trim",
		TurnIndex: 0,
		StartedAt: base,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	rows := []struct {
		idx       int
		id, role  string
		createdAt int64
	}{
		{0, "t-trim-prompt", "user", base},
		{1, "t-trim-reply", "assistant", base + 5},
		{2, "t-trim-anchor", "user", base + 10},
		{3, "t-trim-response", "assistant", base + 20},
	}
	for _, r := range rows {
		if err := s.InsertItem(Item{
			ID:        r.id,
			ThreadID:  "t-trim",
			TurnIndex: 0,
			ItemIndex: r.idx,
			Kind:      "user_text",
			Role:      r.role,
			CreatedAt: r.createdAt,
		}); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	if err := s.UpdateTurnCompleted("t-trim-turn-0", base+100, "end_turn", "am-deleted", `{"in":9}`, ""); err != nil {
		t.Fatalf("settle turn: %v", err)
	}

	if _, err := s.DeleteConversationFromItem("t-trim", "t-trim-anchor"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	turn, ok, err := s.GetTurnByThreadIndex("t-trim", 0)
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if turn.CompletedAt == nil || *turn.CompletedAt != base+5 {
		t.Errorf("completed_at = %v, want last surviving row's created_at %d", turn.CompletedAt, base+5)
	}
	if turn.AssistantMessageID != "" {
		t.Errorf("assistant_message_id = %q, want cleared", turn.AssistantMessageID)
	}
	if turn.TokenUsageJSON != `{"in":9}` || turn.StopReason != "end_turn" {
		t.Errorf("token usage / stop reason should stay: %+v", turn)
	}
}

// TestDeleteConversationFromItemLeavesActiveAnchorTurnAlone — the settle trim
// only rewrites a SETTLED turn row; an anchor turn still active at delete time
// keeps completed_at NULL rather than gaining a fabricated settlement.
func TestDeleteConversationFromItemLeavesActiveAnchorTurnAlone(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-active", base)
	if err := s.InsertTurn(Turn{
		TurnID:    "t-active-turn-0",
		ThreadID:  "t-active",
		TurnIndex: 0,
		StartedAt: base,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	for i, id := range []string{"t-active-prompt", "t-active-anchor"} {
		if err := s.InsertItem(Item{
			ID:        id,
			ThreadID:  "t-active",
			TurnIndex: 0,
			ItemIndex: i,
			Kind:      "user_text",
			Role:      "user",
			CreatedAt: base + int64(i),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	if _, err := s.DeleteConversationFromItem("t-active", "t-active-anchor"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	turn, ok, err := s.GetTurnByThreadIndex("t-active", 0)
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if turn.CompletedAt != nil {
		t.Errorf("active turn gained completed_at %d, want NULL", *turn.CompletedAt)
	}
}

func TestDeleteConversationFromItemMissingAnchorErrors(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-miss", now)

	if _, err := s.DeleteConversationFromItem("t-miss", "no-such-item"); err == nil {
		t.Fatal("expected error for missing anchor item")
	}
}

func createDeleteConversationThread(t *testing.T, s *Store, id string, now int64) {
	t.Helper()
	if err := s.CreateThread(Thread{
		ProjectID:     defaultTestProjectID,
		ID:            id,
		Title:         id,
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

func insertDeleteConversationRows(t *testing.T, s *Store, threadID string, turns int, now int64) {
	t.Helper()
	for turn := 0; turn < turns; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID:    fmt.Sprintf("%s-turn-%d", threadID, turn),
			ThreadID:  threadID,
			TurnIndex: turn,
			StartedAt: now + int64(turn),
		}); err != nil {
			t.Fatalf("insert turn %s %d: %v", threadID, turn, err)
		}
		for i := 0; i < 2; i++ {
			if _, err := s.AppendItem(Item{
				ID:        fmt.Sprintf("%s-item-%d-%d", threadID, turn, i),
				ThreadID:  threadID,
				TurnIndex: turn,
				Kind:      "assistant_text",
				Role:      "assistant",
				CreatedAt: now,
			}); err != nil {
				t.Fatalf("append %s t%d i%d: %v", threadID, turn, i, err)
			}
		}
	}
}

func assertTurnsRemaining(t *testing.T, s *Store, threadID string, want []int) {
	t.Helper()
	rows, err := s.db.Query(`SELECT turn_index FROM turns WHERE thread_id = ? ORDER BY turn_index`, threadID)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var turn int
		if err := rows.Scan(&turn); err != nil {
			t.Fatalf("scan turn: %v", err)
		}
		got = append(got, turn)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("turns remaining: got %v, want %v", got, want)
	}
}

// TestDeleteConversationFromItemAtPickupAnchorKeepsSettle — a non-promoted
// anchor that is the LAST row of its turn (an at-pickup bumped quiet flush,
// whose response ran in the NEXT turn) deletes nothing the turn's settle
// metadata describes. The trim must not fire: completed_at and
// assistant_message_id stay.
func TestDeleteConversationFromItemAtPickupAnchorKeepsSettle(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-pickup", base)
	for turn := 0; turn <= 1; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID: fmt.Sprintf("t-pickup-turn-%d", turn), ThreadID: "t-pickup",
			TurnIndex: turn, StartedAt: base + int64(turn*100),
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
	}
	rows := []struct {
		turn, idx int
		id, role  string
		createdAt int64
	}{
		{0, 0, "t-pickup-prompt", "user", base},
		{0, 1, "t-pickup-reply", "assistant", base + 5},
		{0, 2, "t-pickup-anchor", "user", base + 10}, // bumped to tail at pickup
		{1, 0, "t-pickup-response", "assistant", base + 120},
	}
	for _, r := range rows {
		if err := s.InsertItem(Item{
			ID: r.id, ThreadID: "t-pickup", TurnIndex: r.turn, ItemIndex: r.idx,
			Kind: "user_text", Role: r.role, CreatedAt: r.createdAt,
		}); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	if err := s.UpdateTurnCompleted("t-pickup-turn-0", base+50, "end_turn", "am-kept", `{"in":3}`, ""); err != nil {
		t.Fatalf("settle turn 0: %v", err)
	}

	if _, err := s.DeleteConversationFromItem("t-pickup", "t-pickup-anchor"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	turn, ok, err := s.GetTurnByThreadIndex("t-pickup", 0)
	if err != nil || !ok {
		t.Fatalf("get turn 0: ok=%v err=%v", ok, err)
	}
	if turn.CompletedAt == nil || *turn.CompletedAt != base+50 {
		t.Errorf("completed_at = %v, want untouched %d (no turn content deleted)", turn.CompletedAt, base+50)
	}
	if turn.AssistantMessageID != "am-kept" {
		t.Errorf("assistant_message_id = %q, want untouched am-kept", turn.AssistantMessageID)
	}
}

// TestDeleteConversationFromItemPromotedBoundaryCutsResponse — a promoted
// anchor whose echo stamped a provider-order boundary (mid-loop consumption)
// keeps the interrupted tail (non-user rows at or below the boundary) but
// deletes the response that streamed BELOW it in the same turn (non-user
// rows past the boundary). Because turn content was deleted, the settle
// metadata trims.
func TestDeleteConversationFromItemPromotedBoundaryCutsResponse(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-bound", base)
	if err := s.InsertTurn(Turn{
		TurnID: "t-bound-turn-0", ThreadID: "t-bound", TurnIndex: 0, StartedAt: base,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	promotedMeta, err := itemmeta.MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	boundaryMeta, err := itemmeta.MarkPromotedEchoBoundary(promotedMeta, 5)
	if err != nil {
		t.Fatalf("mark boundary: %v", err)
	}
	// t-bound-subprompt is a parented wire-only user row (a subagent's
	// task prompt nested under a tool_call): user-role but interrupted-
	// tail CONTENT, not a queued message — the cut must keep it.
	rows := []struct {
		idx       int
		id, role  string
		meta      string
		parentID  string
		createdAt int64
	}{
		{0, "t-bound-prompt", "user", "", "", base},
		{1, "t-bound-pre", "assistant", "", "", base + 1},
		{2, "t-bound-anchor", "user", boundaryMeta, "", base + 2},
		{3, "t-bound-queued2", "user", promotedMeta, "", base + 3},
		{4, "t-bound-tail", "assistant", "", "", base + 4},
		{5, "t-bound-subprompt", "user", "", "t-bound-pre", base + 5},
		{6, "t-bound-resp1", "assistant", "", "", base + 6},
		{7, "t-bound-resp2", "assistant", "", "", base + 7},
	}
	for _, r := range rows {
		if err := s.InsertItem(Item{
			ID: r.id, ThreadID: "t-bound", TurnIndex: 0, ItemIndex: r.idx,
			Kind: "user_text", Role: r.role, Meta: r.meta, ParentID: r.parentID,
			CreatedAt: r.createdAt,
		}); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	if err := s.UpdateTurnCompleted("t-bound-turn-0", base+100, "end_turn", "am-resp", `{"in":7}`, ""); err != nil {
		t.Fatalf("settle turn: %v", err)
	}

	if _, err := s.DeleteConversationFromItem("t-bound", "t-bound-anchor"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err := s.ListItems("t-bound")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.ID)
	}
	want := []string{"t-bound-prompt", "t-bound-pre", "t-bound-tail", "t-bound-subprompt"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("surviving items = %v, want %v", got, want)
	}

	turn, ok, err := s.GetTurnByThreadIndex("t-bound", 0)
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if turn.CompletedAt == nil || *turn.CompletedAt != base+5 {
		t.Errorf("completed_at = %v, want last survivor's created_at %d", turn.CompletedAt, base+5)
	}
	if turn.AssistantMessageID != "" {
		t.Errorf("assistant_message_id = %q, want cleared (response deleted)", turn.AssistantMessageID)
	}
	if turn.TokenUsageJSON != `{"in":7}` {
		t.Errorf("token usage should stay: %+v", turn)
	}
}
