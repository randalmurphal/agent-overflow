package app

import (
	"context"
	"encoding/json"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transport"
)

func TestTimelinePagingPreservesLegacyWireAndSelectsScopedRows(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedSyncBindingThread(t, app)
	for _, row := range []store.Item{
		{ID: "agent", ThreadID: thread.ID, TurnIndex: 2, ItemIndex: 0, Kind: "tool_call", ToolName: "Agent", Status: "completed", Role: "assistant"},
		{ID: "child", ThreadID: thread.ID, TurnIndex: 2, ItemIndex: 1, ParentID: "agent", Kind: "assistant_text", Summary: "child transcript", Status: "completed", Role: "assistant"},
	} {
		if err := storetest.WithParentCard(app.store, row, app.store.InsertItem); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(app, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
		t.Fatal(err)
	}
	invoke := func(name string, args ...any) json.RawMessage {
		t.Helper()
		method, ok := dispatcher.LookupName(name)
		if !ok {
			t.Fatalf("missing method %s", name)
		}
		params := make([]json.RawMessage, len(args))
		for i, arg := range args {
			var err error
			params[i], err = json.Marshal(arg)
			if err != nil {
				t.Fatal(err)
			}
		}
		result, failure := dispatcher.InvokeForOrigin(context.Background(), method, params, true)
		if failure != nil {
			t.Fatalf("%s: %+v", name, failure)
		}
		return result
	}
	var main, scoped store.PagedItems
	if err := json.Unmarshal(invoke("ListThreadSliceAround", thread.ID, "", 20, map[string]any{"runWindowRows": 8}), &main); err != nil {
		t.Fatal(err)
	}
	if len(main.Items) == 0 {
		t.Fatal("legacy main page is empty")
	}
	for _, row := range main.Items {
		if row.ParentID != "" {
			t.Fatalf("legacy page leaked child %+v", row)
		}
	}
	if err := json.Unmarshal(invoke("ListThreadSliceAround", thread.ID, "", 20, map[string]any{"runWindowRows": 8, "selection": map[string]any{"scopeRootId": "agent"}}), &scoped); err != nil {
		t.Fatal(err)
	}
	if len(scoped.Items) != 1 || scoped.Items[0].ID != "child" || scoped.Scope == nil || scoped.Scope.Root.ID != "agent" {
		t.Fatalf("scoped page = %+v", scoped)
	}
	invoke("ListItemsBeforeCursor", thread.ID, main.NewestCursor, 20, map[string]any{})
	invoke("ListItemsAfterCursor", thread.ID, main.OldestCursor, 20, map[string]any{})
	invoke("GetThreadUserMessageTicks", thread.ID)
	invoke("GetTimelineUserMessageTicks", thread.ID, map[string]any{"scopeRootId": "agent"})
}
