package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Deltas and patches carry no row, so they name the row's parent. A client
// whose window does not hold the row reads it to tell a row of another
// scope (a subagent child the main window never admits) from a missing one
// (frontend threadItemStreamApply).
func TestStreamDeltasNameTheRowParent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	handle := func(kind provider.EventKind, parent, text string) {
		t.Helper()
		if err := router.Handle(provider.ProviderEvent{
			Kind:            kind,
			ThreadID:        "t1",
			ParentToolUseID: parent,
			Content:         text,
			Timestamp:       time.Now(),
		}); err != nil {
			t.Fatalf("handle %s under %q: %v", kind, parent, err)
		}
	}
	// First chunks open the rows (creation upsert plus a delta); second
	// chunks take the ordinary delta path.
	for _, kind := range []provider.EventKind{provider.EventTextDelta, provider.EventThinking} {
		handle(kind, "toolu_parent", "first ")
		handle(kind, "toolu_parent", "second")
		handle(kind, "", "top ")
		handle(kind, "", "level")
	}

	parentByItem := make(map[string]string)
	for _, event := range filterItemStreamEvents(emissions.snapshot()) {
		if event.Action == itemStreamActionUpsert && event.Item != nil {
			parentByItem[event.Item.ID] = event.Item.ParentID
		}
	}
	deltas := 0
	scoped := 0
	for _, event := range filterItemStreamEvents(emissions.snapshot()) {
		if event.Action != itemStreamActionDelta {
			continue
		}
		deltas++
		want, known := parentByItem[event.ItemID]
		if !known {
			t.Fatalf("delta for %s arrived without its creation upsert", event.ItemID)
		}
		if event.ParentID != want {
			t.Errorf("delta for %s names parent %q, its row has %q", event.ItemID, event.ParentID, want)
		}
		if want == "toolu_parent" {
			scoped++
		}
	}
	if deltas != 8 || scoped != 4 {
		t.Fatalf("deltas = %d (%d scoped), want 8 with 4 under the subagent", deltas, scoped)
	}
}

func TestFieldPatchNamesTheRowParent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	for _, row := range []store.Item{
		{ID: "child", ParentID: "toolu_parent"},
		{ID: "top"},
	} {
		row.ThreadID, row.Kind, row.Role, row.Status = "t1", "assistant_text", "assistant", "streaming"
		row.CreatedAt, row.UpdatedAt = now, now
		appendRow := func(row store.Item) error {
			_, err := st.AppendItem(row)
			return err
		}
		if row.ParentID != "" {
			// A row under a parent is written with the parent's card.
			parented := appendRow
			appendRow = func(row store.Item) error {
				return st.WithSubagentCard(row.ThreadID, row.ParentID, func(card *store.SubagentCard) error {
					row.SubagentCard = card
					return parented(row)
				})
			}
		}
		if err := appendRow(row); err != nil {
			t.Fatalf("append %s: %v", row.ID, err)
		}
	}
	completed := statusCompleted
	for _, id := range []string{"child", "top"} {
		row, found, err := st.GetThreadItem("t1", id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		if err := router.persistItemFieldsAndPatch(row, store.ItemPartialUpdate{Status: &completed}); err != nil {
			t.Fatalf("settle %s: %v", id, err)
		}
	}

	patches := filterItemEventPatches(emissions.snapshot())
	if len(patches) != 2 {
		t.Fatalf("patches = %d, want one per row: %+v", len(patches), patches)
	}
	for _, patch := range patches {
		want := ""
		if patch.ItemID == "child" {
			want = "toolu_parent"
		}
		if patch.ParentID != want {
			t.Errorf("patch for %s names parent %q, want %q", patch.ItemID, patch.ParentID, want)
		}
	}
}
