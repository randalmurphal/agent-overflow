package app

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// TestEmitAttributesItemEventsToTheirScope pins the funnel half of scope
// narrowing: a transcript frame reaches the bus addressed to its thread and
// to the scope of the row it describes, and a scope is derived only for the
// channels the transport narrows by it.
func TestEmitAttributesItemEventsToTheirScope(t *testing.T) {
	app := &App{}
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)

	child := store.Item{ID: "child", ThreadID: "t1", ParentID: "toolu_parent", Kind: "assistant_text"}
	top := store.Item{ID: "top", ThreadID: "t1", Kind: "assistant_text"}
	scoped := map[string]string{"threadId": "t1", "parentId": "toolu_parent"}
	app.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(child))
	app.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(top))
	// EntityFiltered but not TranscriptScopeFiltered: the thread is derived, a
	// parentId in the payload is not read.
	app.emit(eventchan.HighlightSeed, scoped)
	// Neither column: nothing is derived for the bus.
	app.emit(eventchan.ThreadUpdated, scoped)

	type address struct{ channel, key, scope string }
	want := []address{
		{string(eventchan.ProviderItemEvent), "t1", "toolu_parent"},
		{string(eventchan.ProviderItemEvent), "t1", ""},
		{string(eventchan.HighlightSeed), "t1", ""},
		{string(eventchan.ThreadUpdated), "", ""},
	}
	deadline := time.After(2 * time.Second)
	for i, w := range want {
		select {
		case e := <-sub.Events():
			if got := (address{e.Channel, e.EntityKey, e.EntityScope}); got != w {
				t.Fatalf("frame %d addressed %+v, want %+v", i, got, w)
			}
		case <-deadline:
			t.Fatalf("frame %d (%+v) never arrived", i, w)
		}
	}
}
