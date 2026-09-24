package app

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// resyncedThreads lists, in order, the threads the recorded resync frames
// name.
func resyncedThreads(rec *emitRecorder) []string {
	var out []string
	for _, call := range rec.snapshot() {
		evt, ok := call.Data.(triage.ItemStreamEvent)
		if call.Channel != eventchan.ProviderItemEvent.String() || !ok || evt.Action != "resync" {
			continue
		}
		out = append(out, evt.ThreadID)
	}
	return out
}

// TestForkStampMovesPushResyncFrames: a write that moves a pointer fork's
// stamps pushes that fork one resync frame on the item channel, and a
// write that moves none, on a thread no fork reads or past every fork's
// cut, pushes none. Deleting the source pushes the fork its resync, so a
// pane showing it drops the source's rows without being reopened.
func TestForkStampMovesPushResyncFrames(t *testing.T) {
	app := newTestAppWithStore(t)
	app.watchForkStamps()
	for _, id := range []string{"fork-resync-source", "fork-resync-alone"} {
		thread := testThread(id)
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		insertForkTestItems(t, app.store, id)
	}
	fork := testThread("fork-resync-fork")
	fork.ForkedFromThreadID = "fork-resync-source"
	if err := app.store.CreatePointerFork(fork, "fork-resync-source", store.ForkCut{}, func(s string) string { return s }, 1); err != nil {
		t.Fatal(err)
	}
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture

	if err := app.store.UpdateItemMeta("fork-resync-alone", "item-fork-resync-alone-0", `{"edited":true}`); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertItem(store.Item{ID: "late", ThreadID: "fork-resync-source", TurnIndex: 2, Kind: "user_text", Role: "user", Summary: "late"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdateItemMeta("fork-resync-source", "late", `{"edited":true}`); err != nil {
		t.Fatal(err)
	}
	if got := resyncedThreads(rec); len(got) != 0 {
		t.Fatalf("writes that move no fork pushed resyncs for %v", got)
	}

	if err := app.store.UpdateItemMeta("fork-resync-source", "item-fork-resync-source-0", `{"edited":true}`); err != nil {
		t.Fatal(err)
	}
	if got := resyncedThreads(rec); !slices.Equal(got, []string{"fork-resync-fork"}) {
		t.Fatalf("a write below the cut pushed resyncs for %v, want the fork once", got)
	}

	rec.reset()
	if err := app.DeleteThread("fork-resync-source"); err != nil {
		t.Fatal(err)
	}
	got := resyncedThreads(rec)
	if len(got) == 0 {
		t.Fatal("deleting the source pushed the fork no resync")
	}
	for _, id := range got {
		if id != "fork-resync-fork" {
			t.Fatalf("deleting the source pushed resyncs for %v, want the fork only", got)
		}
	}
	// The fork keeps the copy the write below its cut gave it and its
	// divider, which records the deletion; the row it still read from the
	// source went with the source.
	items, err := app.store.ListItems("fork-resync-fork")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "item-fork-resync-source-0" || items[1].ToolName != forkDividerToolName ||
		!strings.Contains(items[1].Meta, `"sourceDeleted":true`) {
		t.Fatalf("detached fork rows = %+v, want the handed-off copy and the marked divider", items)
	}
}
