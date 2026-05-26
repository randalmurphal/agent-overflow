package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

func TestSuccessfulToolTracksWorkspaceRelativeFilePaths(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("load thread: %v", err)
	}
	thread.WorkspacePath = t.TempDir()
	if err := st.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}

	meta, _ := json.Marshal(map[string]any{
		"toolName": "Write",
		"input": map[string]any{
			"file_path": thread.WorkspacePath + "/notes.txt",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "toolu_1",
		ItemType:  "Write",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "toolu_1",
		ItemType:  "Write",
		Meta:      json.RawMessage(`{"is_error":false}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}

	paths, err := st.ListTrackedFiles("t1")
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	if len(paths) != 1 || paths[0] != "notes.txt" {
		t.Fatalf("tracked files = %v, want [notes.txt]", paths)
	}
}

func TestCodexFileChangeClassifierOutputPersistsDiffAndTrackedPath(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	seedOpenTurn(t, router, st, "t1", 0)

	startParams := json.RawMessage(`{"turnId":"turn-1","item":{"id":"patch-1","type":"fileChange","changes":[{"path":"src/app.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1,2 @@\n package main\n+const value = 1"}],"status":"inProgress"}}`)
	startEvents := codex.ClassifyNotification("t1", "item/started", startParams)
	if len(startEvents) != 1 {
		t.Fatalf("start events = %d, want 1: %+v", len(startEvents), startEvents)
	}
	if err := router.Handle(startEvents[0]); err != nil {
		t.Fatalf("handle fileChange start: %v", err)
	}

	completeParams := json.RawMessage(`{"turnId":"turn-1","item":{"id":"patch-1","type":"fileChange","changes":[{"path":"src/app.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1,2 @@\n package main\n+const value = 1"}],"status":"completed"}}`)
	completeEvents := codex.ClassifyNotification("t1", "item/completed", completeParams)
	if len(completeEvents) != 1 {
		t.Fatalf("complete events = %d, want 1: %+v", len(completeEvents), completeEvents)
	}
	if err := router.Handle(completeEvents[0]); err != nil {
		t.Fatalf("handle fileChange complete: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "patch-1")
	if err != nil {
		t.Fatalf("get fileChange item: %v", err)
	}
	if !found {
		t.Fatal("expected persisted fileChange item")
	}
	if item.PayloadKind != toolResultPayloadKind {
		t.Fatalf("payload kind = %q, want %q", item.PayloadKind, toolResultPayloadKind)
	}
	var itemMeta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item.Meta), &itemMeta); err != nil {
		t.Fatalf("unmarshal item meta: %v", err)
	}
	if _, ok := itemMeta["item"]; ok {
		t.Fatalf("stored item meta should not retain raw fileChange item: %s", item.Meta)
	}
	var meta ToolResultMeta
	if err := json.Unmarshal([]byte(item.PayloadMeta), &meta); err != nil {
		t.Fatalf("unmarshal tool result meta: %v", err)
	}
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", meta.InlineDiff)
	}
	if len(meta.InlineDiff.Files) != 1 || meta.InlineDiff.Files[0].Path != "src/app.go" {
		t.Fatalf("inline diff files = %+v, want src/app.go", meta.InlineDiff.Files)
	}

	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get diff payload: %v", err)
	}
	diff := string(data)
	if !strings.Contains(diff, "diff --git a/src/app.go b/src/app.go") ||
		!strings.Contains(diff, "+const value = 1") {
		t.Fatalf("diff payload did not preserve unified diff: %q", diff)
	}

	paths, err := st.ListTrackedFiles("t1")
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	if len(paths) != 1 || paths[0] != "src/app.go" {
		t.Fatalf("tracked files = %v, want [src/app.go]", paths)
	}
}

func TestCodexCompletionOnlyFileChangeTracksPathOnlyOnSuccess(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      string
		wantTracked bool
	}{
		{name: "completed", status: "completed", wantTracked: true},
		{name: "failed", status: "failed", wantTracked: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createToolResultThread(t, st, "t1", t.TempDir())
			seedOpenTurn(t, router, st, "t1", 0)

			params := json.RawMessage(`{"turnId":"turn-1","item":{"id":"patch-1","type":"fileChange","changes":[{"path":"src/app.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1,2 @@\n package main\n+const value = 1"}],"status":"` + tc.status + `"}}`)
			events := codex.ClassifyNotification("t1", "item/completed", params)
			if len(events) != 1 {
				t.Fatalf("complete events = %d, want 1: %+v", len(events), events)
			}
			if err := router.Handle(events[0]); err != nil {
				t.Fatalf("handle fileChange complete: %v", err)
			}

			paths, err := st.ListTrackedFiles("t1")
			if err != nil {
				t.Fatalf("list tracked files: %v", err)
			}
			if tc.wantTracked {
				if len(paths) != 1 || paths[0] != "src/app.go" {
					t.Fatalf("tracked files = %v, want [src/app.go]", paths)
				}
				return
			}
			if len(paths) != 0 {
				t.Fatalf("tracked files = %v, want none", paths)
			}
		})
	}
}

func TestFailedToolDropsStagedTrackedPath(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("load thread: %v", err)
	}
	thread.WorkspacePath = t.TempDir()
	if err := st.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{
		"toolName": "Edit",
		"input": map[string]any{
			"file_path": thread.WorkspacePath + "/broken.txt",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "toolu_1",
		ItemType:  "Edit",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "toolu_1",
		ItemType:  "Edit",
		Meta:      json.RawMessage(`{"is_error":true}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}

	paths, err := st.ListTrackedFiles("t1")
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("tracked files = %v, want none", paths)
	}
}
