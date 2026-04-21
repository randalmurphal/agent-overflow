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
		if eventName != "provider:item_upsert" {
			return
		}
		item, ok := data.(store.Item)
		if !ok {
			t.Fatalf("provider:item_upsert payload type = %T, want store.Item", data)
		}
		if item.Kind == "error" {
			items <- item
		}
	}, app.highlighter)
	return items
}
