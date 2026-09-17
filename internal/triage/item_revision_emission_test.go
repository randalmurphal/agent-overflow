package triage

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// revEmission records one pushed item event next to what SQLite held for
// the same row at the instant it was pushed.
type revEmission struct {
	action    string
	itemID    string
	kind      string
	wireRev   int64
	storedRev int64
	// wireIsStoredRow reports whether the pushed row was a faithful copy
	// of the stored row. Only meaningful for upserts.
	wireIsStoredRow bool
	found           bool
	readErr         error
}

// revWitness is an emit callback that checks each `upsert` against the
// store as it goes out, instead of after the turn. Checking later would
// prove nothing: the same row is written again by the next event, so a
// stale wire rev would look like a fresh one that the store has since
// moved past.
type revWitness struct {
	mu   sync.Mutex
	st   *store.Store
	seen []revEmission
}

func (w *revWitness) emit(channel eventchan.Channel, data any) {
	if channel.String() != "provider:item_event" {
		return
	}
	evt, ok := data.(ItemStreamEvent)
	if !ok {
		return
	}
	record := revEmission{action: evt.Action, kind: evt.Kind}
	threadID := evt.ThreadID
	switch {
	case evt.Action == itemStreamActionUpsert && evt.Item != nil:
		record.itemID = evt.Item.ID
		record.kind = evt.Item.Kind
		record.wireRev = evt.Item.Rev
	case evt.Action == itemStreamActionPatch && evt.Patch != nil:
		record.itemID = evt.ItemID
		record.wireRev = evt.Patch.Rev
	default:
		return
	}
	// The stored row is what a PAGE reads: hydrated and decorated. An
	// anchor's descendant aggregate is part of its read, so an upsert
	// that carried the undecorated write read-back would be an altered
	// row claiming the stored revision.
	rows, err := w.st.ListWireItems(threadID, []string{record.itemID})
	var stored store.Item
	if err == nil && len(rows) == 1 {
		stored, record.found = rows[0], true
	}
	record.storedRev, record.readErr = stored.Rev, err
	if evt.Item != nil {
		// The row a faithful upsert carries is the stored row put through
		// the same wire projection. When it is not — the streaming reveal
		// blanks the summary on purpose — the event is not a copy of the
		// stored row and must say so with its revision.
		projected := itemwire.Project(stored, true)
		record.wireIsStoredRow = reflect.DeepEqual(*evt.Item, projected)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, record)
}

func (w *revWitness) snapshot() []revEmission {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]revEmission(nil), w.seen...)
}

// TestEmittedItemEventsCarryStoredItemRev is the emit-side half of the
// window digest contract (docs/architecture/thread-replica-sync.md §3.1).
// A client builds the held window it sends to SyncThreadWindow out of the
// rows it was PUSHED, so a `rev` that is right in SQLite and wrong on the
// wire either costs a page on every reopen (too old) or proves a window
// fresh that is not (too new).
//
// It drives a whole mocked turn — assistant text, a tool call and its
// completion, a second text block, turn settle — and checks every item
// event against SQLite AT THE MOMENT IT IS PUSHED. Checking afterwards
// would prove nothing: the next event rewrites the row, so a stale wire
// rev would look like a fresh one the store has since moved past.
//
// Three rules, one per shape of event:
//
//   - an upsert carrying a faithful copy of the stored row must carry that
//     row's revision, which is only possible if the emit site sends a row
//     read back inside its write transaction (the triggers stamp `rev`
//     after the statement, so an input struct is stale or zero);
//   - an upsert whose row the emitter altered on purpose — the streaming
//     reveal blanks the summary so the text can arrive as deltas — must
//     carry store.UnstampedItemRev, because the client's copy is not the
//     stored row until the deltas and the settle patch land;
//   - a patch must carry the revision its own write produced, since the
//     client folds it into the row it holds and that fold is what makes
//     the row a copy of the stored one again.
func TestEmittedItemEventsCarryStoredItemRev(t *testing.T) {
	st := storetest.Clone(t)
	witness := &revWitness{st: st}
	router := NewRouter(st, witness.emit)
	t.Cleanup(router.flushAllUsage)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})

	steps := []struct {
		name  string
		event provider.ProviderEvent
	}{
		{"turn start", provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
		}},
		{"text delta", provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", Content: "first answer", Timestamp: time.Now(),
		}},
		{"tool start", provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-1",
			ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
		}},
		{"tool complete", provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "tool-1",
			Meta: completeMeta, Content: "stdout line", Timestamp: time.Now(),
		}},
		{"second text delta", provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", Content: "second answer", Timestamp: time.Now(),
		}},
		{"turn complete", provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1",
			TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
		}},
	}
	for _, step := range steps {
		if err := router.Handle(step.event); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		router.WaitForPendingSettles()
	}

	pushed := witness.snapshot()
	rowsSeen := map[string]bool{}
	patches := 0
	unstamped := 0
	for i, e := range pushed {
		if e.readErr != nil {
			t.Fatalf("%s %d (%s): read back %s: %v", e.action, i, e.kind, e.itemID, e.readErr)
		}
		if !e.found {
			t.Errorf("%s %d pushed row %s (%s) that is not in the store", e.action, i, e.itemID, e.kind)
			continue
		}
		switch {
		case e.action == itemStreamActionUpsert && !e.wireIsStoredRow:
			unstamped++
			if e.wireRev != store.UnstampedItemRev {
				t.Errorf("upsert %d: row %s (%s) was altered for the wire but claimed rev %d (stored %d); it must carry store.UnstampedItemRev",
					i, e.itemID, e.kind, e.wireRev, e.storedRev)
			}
		case e.action == itemStreamActionUpsert:
			rowsSeen[e.itemID] = true
			if e.wireRev != e.storedRev {
				t.Errorf("upsert %d: row %s (%s): wire rev = %d, stored rev at emit = %d — the emit site is not sending a row read back inside its write transaction",
					i, e.itemID, e.kind, e.wireRev, e.storedRev)
			}
		case e.action == itemStreamActionPatch:
			patches++
			rowsSeen[e.itemID] = true
			if e.wireRev != e.storedRev {
				t.Errorf("patch %d: row %s: wire rev = %d, stored rev at emit = %d — the patch is not carrying the revision its own write produced",
					i, e.itemID, e.wireRev, e.storedRev)
			}
		}
	}

	// The turn must actually have produced each shape; an emit-site
	// refactor that stopped pushing one would otherwise pass vacuously.
	if patches == 0 {
		t.Error("the turn emitted no patches, so the patch rule was never exercised")
	}
	if unstamped == 0 {
		t.Error("the turn emitted no altered wire rows, so the unstamped rule was never exercised")
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) < 3 {
		t.Fatalf("turn produced %d rows, want at least 3 (two text blocks, the tool call): %+v", len(items), items)
	}
	for _, row := range items {
		if !rowsSeen[row.ID] {
			t.Errorf("row %s (%s) was persisted but no event ever described it at its stored revision", row.ID, row.Kind)
		}
		if row.Rev <= 0 {
			t.Errorf("row %s (%s) persisted with rev %d — the history triggers did not stamp it", row.ID, row.Kind, row.Rev)
		}
	}
}
