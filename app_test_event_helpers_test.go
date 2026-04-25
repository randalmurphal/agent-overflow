package main

import (
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func collectErrorItemUpserts(t *testing.T, app *App, buffer int) chan store.Item {
	t.Helper()

	items := make(chan store.Item, buffer)
	app.triage = triage.NewRouter(app.store, func(eventName string, data any) {
		if eventName != "provider:item_event" {
			return
		}
		event, ok := data.(triage.ItemStreamEvent)
		if !ok {
			t.Fatalf("provider:item_event payload type = %T, want triage.ItemStreamEvent", data)
		}
		if event.Action != "upsert" || event.Item == nil {
			return
		}
		item := *event.Item
		if item.Kind == "error" {
			items <- item
		}
	})
	return items
}

// itemFromItemStreamEvent peels the upsert payload out of a
// provider:item_event emission. Returns the item and true on a
// well-formed upsert; (zero, false) on any other action or shape so
// tests can decide whether to fail or skip.
func itemFromItemStreamEvent(data any) (store.Item, bool) {
	event, ok := data.(triage.ItemStreamEvent)
	if !ok || event.Action != "upsert" || event.Item == nil {
		return store.Item{}, false
	}
	return *event.Item, true
}
