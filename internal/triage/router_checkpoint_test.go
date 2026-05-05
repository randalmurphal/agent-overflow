package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
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
