package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestFileChangeToolResultUpgradesFromTurnDiff(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	payloadID := toolResultPayloadID(providerScopedItemID("item-file-change"))

	startMeta := json.RawMessage(`{
		"item": {
			"id": "item-file-change",
			"type": "file_change",
			"title": "File change",
			"detail": "Editing src/app.ts",
			"data": {
				"item": {
					"changes": [
						{
							"path": "src/app.ts",
							"kind": {"type": "update", "move_path": null}
						}
					]
				}
			}
		}
	}`)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-file-change",
		ItemType:  "file_change",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	meta := readToolResultMeta(t, st, payloadID)
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "summary_only" {
		t.Fatalf("expected summary_only inline diff, got %+v", meta.InlineDiff)
	}

	turnDiff := strings.Join([]string{
		"diff --git a/src/app.ts b/src/app.ts",
		"--- a/src/app.ts",
		"+++ b/src/app.ts",
		"@@ -1 +1,2 @@",
		" export const value = 1;",
		"+export const next = 2;",
		"",
		"diff --git a/src/other.ts b/src/other.ts",
		"--- a/src/other.ts",
		"+++ b/src/other.ts",
		"@@ -0,0 +1 @@",
		"+extra",
	}, "\n")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   turnDiff,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle diff: %v", err)
	}

	upgraded := readToolResultMeta(t, st, payloadID)
	if upgraded.InlineDiff == nil || upgraded.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("expected exact_patch inline diff, got %+v", upgraded.InlineDiff)
	}
	if len(upgraded.InlineDiff.Files) != 1 || upgraded.InlineDiff.Files[0].Path != "src/app.ts" {
		t.Fatalf("unexpected upgraded files: %+v", upgraded.InlineDiff.Files)
	}

	data, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if strings.Contains(string(data), "src/other.ts") {
		t.Fatalf("expected filtered tool patch, got %q", string(data))
	}
	if !strings.Contains(string(data), "src/app.ts") {
		t.Fatalf("expected upgraded patch to contain src/app.ts, got %q", string(data))
	}
}

func TestFileChangeToolResultDoesNotOverwriteExistingExactPatch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	payloadID := toolResultPayloadID(providerScopedItemID("item-file-change"))

	startMeta := json.RawMessage(`{
		"item": {
			"id": "item-file-change",
			"type": "file_change",
			"title": "File change",
			"detail": "Editing src/app.ts",
			"data": {
				"item": {
					"changes": [
						{
							"path": "src/app.ts",
							"kind": {"type": "update", "move_path": null},
							"diff": "@@ -1 +1,2 @@\n export const value = 1;\n+export const exactToolPatch = 2;"
						}
					]
				}
			}
		}
	}`)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-file-change",
		ItemType:  "file_change",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	initial, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("get initial payload data: %v", err)
	}
	if !strings.Contains(string(initial), "exactToolPatch") {
		t.Fatalf("expected initial exact patch, got %q", string(initial))
	}

	turnDiff := strings.Join([]string{
		"diff --git a/src/app.ts b/src/app.ts",
		"--- a/src/app.ts",
		"+++ b/src/app.ts",
		"@@ -1 +1,2 @@",
		" export const value = 1;",
		"+export const nativeTurnPatch = 3;",
	}, "\n")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   turnDiff,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle diff: %v", err)
	}

	after, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("get payload data after diff: %v", err)
	}
	if !strings.Contains(string(after), "exactToolPatch") {
		t.Fatalf("expected exact tool patch to remain, got %q", string(after))
	}
	if strings.Contains(string(after), "nativeTurnPatch") {
		t.Fatalf("expected native turn patch not to overwrite exact tool patch, got %q", string(after))
	}
}

func TestExtractFileChangeToolResultNormalizesAbsoluteWorkspacePaths(t *testing.T) {
	workspace := t.TempDir()
	absolutePath := filepath.Join(workspace, "src", "app.ts")

	raw, err := json.Marshal(map[string]any{
		"item": map[string]any{
			"type":  "file_change",
			"title": "File change",
			"data": map[string]any{
				"item": map[string]any{
					"changes": []map[string]any{{
						"path": absolutePath,
						"kind": map[string]any{"type": "update", "move_path": nil},
					}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw tool result: %v", err)
	}

	meta, _, ok := extractFileChangeToolResult(raw, workspace)
	if !ok {
		t.Fatal("expected absolute-path tool result extraction to succeed")
	}
	if meta.InlineDiff == nil || len(meta.InlineDiff.Files) != 1 {
		t.Fatalf("unexpected inline diff meta: %+v", meta.InlineDiff)
	}
	if meta.InlineDiff.Files[0].Path != "src/app.ts" {
		t.Fatalf("expected normalized relative path, got %+v", meta.InlineDiff.Files[0])
	}
}

func TestCommandExecutionToolResultPersistsExactDeletePatch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	payloadID := toolResultPayloadID(providerScopedItemID("item-command-rm"))

	path := filepath.Join(workspace, "src", "remove.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("export const removed = true;\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	startMeta := json.RawMessage(`{
		"item": {
			"id": "item-command-rm",
			"type": "command_execution",
			"title": "Run command",
			"data": {
				"item": {
					"command": "/usr/bin/zsh -lc 'rm src/remove.ts'"
				}
			}
		}
	}`)
	completeMeta := json.RawMessage(`{
		"item": {
			"id": "item-command-rm",
			"type": "command_execution",
			"title": "Run command",
			"data": {
				"item": {
					"command": "/usr/bin/zsh -lc 'rm src/remove.ts'",
					"exitCode": 0
				}
			}
		}
	}`)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-command-rm",
		ItemType:  "command_execution",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "item-command-rm",
		ItemType:  "command_execution",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}

	meta := readToolResultMeta(t, st, payloadID)
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("expected exact_patch inline diff, got %+v", meta.InlineDiff)
	}
	if len(meta.InlineDiff.Files) != 1 || meta.InlineDiff.Files[0].Path != "src/remove.ts" {
		t.Fatalf("unexpected inline diff files: %+v", meta.InlineDiff.Files)
	}

	data, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if !strings.Contains(string(data), "deleted file mode 100644") {
		t.Fatalf("expected delete patch, got %q", string(data))
	}
}

func TestCommandExecutionToolResultSkipsDependentAndFailedCommands(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)

	oldPath := filepath.Join(workspace, "src", "old.ts")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("export const oldName = true;\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dependentStart := json.RawMessage(`{
		"item": {
			"id": "item-command-dependent",
			"type": "command_execution",
			"title": "Run command",
			"data": {
				"item": {
					"command": "mv src/old.ts src/new.ts && rm src/new.ts"
				}
			}
		}
	}`)
	failedStart := json.RawMessage(`{
		"item": {
			"id": "item-command-failed",
			"type": "command_execution",
			"title": "Run command",
			"data": {
				"item": {
					"command": "rm src/old.ts"
				}
			}
		}
	}`)
	failedComplete := json.RawMessage(`{
		"item": {
			"id": "item-command-failed",
			"type": "command_execution",
			"title": "Run command",
			"data": {
				"item": {
					"command": "rm src/old.ts",
					"exitCode": 1
				}
			}
		}
	}`)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-command-dependent",
		ItemType:  "command_execution",
		Meta:      dependentStart,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle dependent start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-command-failed",
		ItemType:  "command_execution",
		Meta:      failedStart,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle failed start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "item-command-failed",
		ItemType:  "command_execution",
		Meta:      failedComplete,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle failed complete: %v", err)
	}

	if _, err := st.GetPayloadMeta(toolResultPayloadID(providerScopedItemID("item-command-dependent"))); err == nil {
		t.Fatal("expected no payload for dependent command")
	}
	if _, err := st.GetPayloadMeta(toolResultPayloadID(providerScopedItemID("item-command-failed"))); err == nil {
		t.Fatal("expected no payload for failed command")
	}
}

func createToolResultThread(t *testing.T, st *store.Store, id, workspace string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     triageTestProjectID,
		Title:         "Test",
		Provider:      "codex",
		WorkspacePath: workspace,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

func readToolResultMeta(t *testing.T, st *store.Store, payloadID string) ToolResultMeta {
	t.Helper()
	pm, err := st.GetPayloadMeta(payloadID)
	if err != nil {
		t.Fatalf("get payload meta: %v", err)
	}
	var meta ToolResultMeta
	if err := json.Unmarshal([]byte(pm.Meta), &meta); err != nil {
		t.Fatalf("unmarshal tool result meta: %v", err)
	}
	return meta
}
