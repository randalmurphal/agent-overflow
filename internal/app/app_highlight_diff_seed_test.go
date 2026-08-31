package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/store"
)

const diffSeedTestPatch = "diff --git a/src/app.py b/src/app.py\n--- a/src/app.py\n+++ b/src/app.py\n@@ -1 +1,2 @@\n def f():\n+    pass"

var diffSpanItemIndex atomic.Int32

func insertDiffSpanPayload(t *testing.T, app *App, threadID, itemID, payloadID, kind, data string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{ID: itemID, ThreadID: threadID, ItemIndex: int(diffSpanItemIndex.Add(1)), Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: payloadID, CreatedAt: now, UpdatedAt: now}, store.Payload{ID: payloadID, Kind: kind, Data: []byte(data), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestHighlightDiffCoordinationPersistsSnapshotsSpansAndEvent(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return true }
	workspace := t.TempDir()
	thread := testThread("thread-highlight-diff")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "app.py"), []byte("def f():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{ID: "item", ThreadID: thread.ID, Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "payload", CreatedAt: now, UpdatedAt: now}, store.Payload{ID: "payload", Kind: "tool_result", Data: []byte(diffSeedTestPatch), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []HighlightDiffSeedEvent
	app.testEmitHook = func(name string, data any) {
		if name != "highlight:diff_seed" {
			return
		}
		if event, ok := data.(HighlightDiffSeedEvent); ok {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
	}
	app.observeDiffPayloadPersisted(thread.ID, "payload", []string{diffSeedTestPatch}, diffSeedTestPatch)

	deadline := time.Now().Add(10 * time.Second)
	var persisted PersistedPatchSpans
	for {
		blob, err := app.store.GetPayloadSpans(thread.ID, "payload")
		if err != nil {
			t.Fatal(err)
		}
		if blob != "" {
			if err := json.Unmarshal([]byte(blob), &persisted); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for diff spans")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if persisted.Version != highlight.SchemaVersion() || len(persisted.Files) != 1 || !persisted.Files[0].Primed {
		t.Fatalf("persisted = %+v", persisted)
	}
	content, found, err := app.store.GetEditFileSnapshot(thread.ID, "payload", "src/app.py")
	if err != nil || !found || content != "def f():\n    pass\n" {
		t.Fatalf("snapshot = %q found=%v err=%v", content, found, err)
	}
	for {
		mu.Lock()
		done := len(events) == 1
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for diff seed event")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].ThreadID != thread.ID || len(events[0].Files) != 1 {
		t.Fatalf("events = %+v", events)
	}

	payload, err := app.GetPayloadData(thread.ID, "payload")
	if err != nil || len(payload.PatchSpans) != 1 {
		t.Fatalf("payload spans = %+v err=%v", payload.PatchSpans, err)
	}
}
