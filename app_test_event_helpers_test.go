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

func itemFromItemStreamEnvelope(data any) (store.Item, bool) {
	env, ok := data.(SeqEnvelope)
	if !ok {
		return store.Item{}, false
	}
	event, ok := env.Data.(triage.ItemStreamEvent)
	if !ok || event.Action != "upsert" || event.Item == nil {
		return store.Item{}, false
	}
	return *event.Item, true
}
