package triage

import (
	"encoding/json"
	"fmt"
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
	payloadID := ToolResultPayloadID("item-file-change")

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

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var toolItem store.Item
	for _, it := range items {
		if it.PayloadID == payloadID {
			toolItem = it
			break
		}
	}
	if toolItem.ID == "" {
		t.Fatalf("upgraded tool_result item not found")
	}
	if toolItem.Summary == "" {
		t.Fatalf("upgraded tool_result item summary was empty")
	}
	if toolItem.Summary != upgraded.Title {
		t.Fatalf("tool item summary = %q, want title %q", toolItem.Summary, upgraded.Title)
	}
}

func TestFileChangeToolResultDoesNotOverwriteExistingExactPatch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	payloadID := ToolResultPayloadID("item-file-change")

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

	meta, _, ok := ExtractFileChangeToolResult(raw, workspace)
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

func TestExtractFileChangeToolResultDropsUnsafePaths(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.ts")

	raw, err := json.Marshal(map[string]any{
		"item": map[string]any{
			"type": "fileChange",
			"changes": []map[string]any{
				{"path": "src/app.ts", "kind": map[string]any{"type": "update", "move_path": nil}},
				{"path": "../escape.ts", "kind": map[string]any{"type": "update", "move_path": nil}},
				{"path": outside, "kind": map[string]any{"type": "update", "move_path": nil}},
				{"path": ".git/config", "kind": map[string]any{"type": "update", "move_path": nil}},
				{"path": ":(top)src/app.ts", "kind": map[string]any{"type": "update", "move_path": nil}},
				{"path": "bad\u0000path.ts", "kind": map[string]any{"type": "update", "move_path": nil}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw tool result: %v", err)
	}

	meta, _, ok := ExtractFileChangeToolResult(raw, workspace)
	if !ok {
		t.Fatal("expected valid fileChange entry to extract")
	}
	if meta.InlineDiff == nil || len(meta.InlineDiff.Files) != 1 {
		t.Fatalf("unexpected inline diff meta: %+v", meta.InlineDiff)
	}
	if meta.InlineDiff.Files[0].Path != "src/app.ts" {
		t.Fatalf("files = %+v, want only src/app.ts", meta.InlineDiff.Files)
	}
}

func TestExtractFileChangeToolResultReadsCurrentCodexFileChangeShape(t *testing.T) {
	raw := json.RawMessage(`{
		"item": {
			"id": "patch-1",
			"type": "fileChange",
			"changes": [
				{
					"path": "src/app.ts",
					"kind": {"type": "update", "move_path": null},
					"diff": "@@ -1 +1,2 @@\n const value = 1;\n+const next = 2;"
				},
				{
					"path": "src/old.ts",
					"kind": {"type": "update", "move_path": "src/new.ts"},
					"diff": "@@ -1 +1 @@\n-old\n+new"
				}
			],
			"status": "completed"
		}
	}`)

	meta, diffData, ok := ExtractFileChangeToolResult(raw, "")
	if !ok {
		t.Fatal("expected current fileChange shape to extract")
	}
	if meta.Title != "Edited 2 files (+2 -1)" {
		t.Fatalf("title = %q, want %q", meta.Title, "Edited 2 files (+2 -1)")
	}
	if meta.Preview != meta.Title {
		t.Fatalf("preview = %q, want title %q", meta.Preview, meta.Title)
	}
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", meta.InlineDiff)
	}
	if len(meta.InlineDiff.Files) != 2 {
		t.Fatalf("files = %+v, want 2", meta.InlineDiff.Files)
	}
	for _, file := range meta.InlineDiff.Files {
		if file.PreviewPatch == "" {
			t.Fatalf("preview patch missing for %+v", file)
		}
		if file.PreviewLineCount == 0 {
			t.Fatalf("preview line count missing for %+v", file)
		}
	}
	renamed := meta.InlineDiff.Files[1]
	if renamed.Path != "src/new.ts" || renamed.PreviousPath != "src/old.ts" || renamed.Kind != "renamed" {
		t.Fatalf("rename metadata = %+v", renamed)
	}
	diff := string(diffData)
	if !strings.Contains(diff, "rename from src/old.ts") || !strings.Contains(diff, "rename to src/new.ts") {
		t.Fatalf("expected rename headers in exact patch, got %q", diff)
	}
}

func TestExtractFileChangeToolResultAcceptsMatchingFullPatch(t *testing.T) {
	raw := json.RawMessage(`{
		"item": {
			"id": "patch-1",
			"type": "fileChange",
			"changes": [
				{
					"path": "src/app.ts",
					"kind": {"type": "update", "move_path": null},
					"diff": "diff --git a/src/app.ts b/src/app.ts\n--- a/src/app.ts\n+++ b/src/app.ts\n@@ -1 +1,2 @@\n const value = 1;\n+const next = 2;"
				}
			]
		}
	}`)

	meta, diffData, ok := ExtractFileChangeToolResult(raw, "")
	if !ok {
		t.Fatal("expected matching full-patch fileChange extraction to succeed")
	}
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", meta.InlineDiff)
	}
	if !strings.Contains(string(diffData), "diff --git a/src/app.ts b/src/app.ts") {
		t.Fatalf("expected full patch in payload, got %q", string(diffData))
	}
}

func TestExtractFileChangeToolResultRejectsMismatchedFullPatchPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{
			name: "different safe path",
			diff: "diff --git a/src/other.ts b/src/other.ts\n--- a/src/other.ts\n+++ b/src/other.ts\n@@ -1 +1 @@\n-old\n+new",
		},
		{
			name: "unsafe git path",
			diff: "diff --git a/.git/config b/.git/config\n--- a/.git/config\n+++ b/.git/config\n@@ -1 +1 @@\n-old\n+new",
		},
		{
			name: "multiple sections",
			diff: "diff --git a/src/app.ts b/src/app.ts\n--- a/src/app.ts\n+++ b/src/app.ts\n@@ -1 +1 @@\n-old\n+new\n\ndiff --git a/src/other.ts b/src/other.ts\n--- a/src/other.ts\n+++ b/src/other.ts\n@@ -1 +1 @@\n-old\n+new",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"item": map[string]any{
					"id":   "patch-1",
					"type": "fileChange",
					"changes": []map[string]any{{
						"path": "src/app.ts",
						"kind": map[string]any{"type": "update", "move_path": nil},
						"diff": tc.diff,
					}},
				},
			})
			if err != nil {
				t.Fatalf("marshal raw: %v", err)
			}

			meta, diffData, ok := ExtractFileChangeToolResult(raw, "")
			if !ok {
				t.Fatal("expected summary-only fileChange extraction to succeed")
			}
			if meta.InlineDiff == nil || meta.InlineDiff.Availability != "summary_only" {
				t.Fatalf("inline diff = %+v, want summary_only", meta.InlineDiff)
			}
			if len(diffData) != 0 {
				t.Fatalf("diff data = %q, want empty for rejected full patch", string(diffData))
			}
		})
	}
}

func TestBuildInlineDiffFromChangesStoresLineBoundedFilePreviews(t *testing.T) {
	var largeContent strings.Builder
	for i := 1; i <= 35; i++ {
		if i > 1 {
			largeContent.WriteByte('\n')
		}
		largeContent.WriteString(fmt.Sprintf("line-%02d", i))
	}

	inlineDiff, combinedDiff := buildInlineDiffFromChanges([]fileChange{
		{Path: "src/large.txt", Kind: "added", Diff: largeContent.String()},
		{Path: "src/small.txt", Kind: "added", Diff: "small"},
	})
	if inlineDiff == nil || len(inlineDiff.Files) != 2 {
		t.Fatalf("inline diff = %+v, want two files", inlineDiff)
	}
	if combinedDiff == "" {
		t.Fatal("combined diff missing")
	}

	large := inlineDiff.Files[0]
	if large.PreviewLineCount != inlineDiffPreviewLineCount {
		t.Fatalf("large preview line count = %d, want %d", large.PreviewLineCount, inlineDiffPreviewLineCount)
	}
	if !large.PreviewTruncated {
		t.Fatalf("large preview truncated = false, want true")
	}
	if !strings.Contains(large.PreviewPatch, "+line-30") {
		t.Fatalf("large preview missing capped final line: %q", large.PreviewPatch)
	}
	if strings.Contains(large.PreviewPatch, "+line-31") {
		t.Fatalf("large preview includes line beyond cap: %q", large.PreviewPatch)
	}

	small := inlineDiff.Files[1]
	if small.PreviewPatch == "" || !strings.Contains(small.PreviewPatch, "+small") {
		t.Fatalf("small preview patch = %q, want exact per-file preview", small.PreviewPatch)
	}
	if small.PreviewTruncated {
		t.Fatalf("small preview truncated = true, want false")
	}
}

func TestBuildInlineDiffFromChangesCapsPreviewFiles(t *testing.T) {
	changes := make([]fileChange, 0, inlineDiffPreviewFileCount+3)
	for i := 0; i < inlineDiffPreviewFileCount+3; i++ {
		changes = append(changes, fileChange{
			Path: fmt.Sprintf("src/file-%02d.txt", i),
			Kind: "added",
			Diff: fmt.Sprintf("line-%02d", i),
		})
	}

	inlineDiff, combinedDiff := buildInlineDiffFromChanges(changes)
	if inlineDiff == nil {
		t.Fatal("inline diff = nil")
	}
	if inlineDiff.Availability != "exact_patch" {
		t.Fatalf("availability = %q, want exact_patch", inlineDiff.Availability)
	}
	if len(inlineDiff.Files) != inlineDiffPreviewFileCount {
		t.Fatalf("preview files = %d, want %d", len(inlineDiff.Files), inlineDiffPreviewFileCount)
	}
	if inlineDiff.TotalFiles != len(changes) {
		t.Fatalf("total files = %d, want %d", inlineDiff.TotalFiles, len(changes))
	}
	if inlineDiff.OmittedFiles != 3 || !inlineDiff.FilesTruncated {
		t.Fatalf("truncation metadata = omitted %d truncated %v, want omitted 3 truncated true", inlineDiff.OmittedFiles, inlineDiff.FilesTruncated)
	}
	if !strings.Contains(combinedDiff, "src/file-27.txt") {
		t.Fatal("combined exact patch should retain omitted file content")
	}
	if got := fileChangeTitle(inlineDiff); got != "Edited 28 files (+28 -0)" {
		t.Fatalf("title = %q, want total-file count", got)
	}
}

func TestLineBoundedDiffPreviewCountsPatchLikeBodyLines(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 35; i++ {
		if i > 1 {
			content.WriteByte('\n')
		}
		content.WriteString(fmt.Sprintf("++patch-looking-line-%02d", i))
	}

	inlineDiff, _ := buildInlineDiffFromChanges([]fileChange{{
		Path: "src/patch-looking.txt",
		Kind: "added",
		Diff: content.String(),
	}})
	if inlineDiff == nil || len(inlineDiff.Files) != 1 {
		t.Fatalf("inline diff = %+v, want one file", inlineDiff)
	}

	file := inlineDiff.Files[0]
	if file.PreviewLineCount != inlineDiffPreviewLineCount {
		t.Fatalf("preview line count = %d, want %d", file.PreviewLineCount, inlineDiffPreviewLineCount)
	}
	if !file.PreviewTruncated {
		t.Fatal("preview truncated = false, want true")
	}
	if !strings.Contains(file.PreviewPatch, "+++patch-looking-line-30") {
		t.Fatalf("preview missing capped hunk body line: %q", file.PreviewPatch)
	}
	if strings.Contains(file.PreviewPatch, "+++patch-looking-line-31") {
		t.Fatalf("preview includes body line beyond cap: %q", file.PreviewPatch)
	}
}

func TestBuildExactInlineDiffPreservesRenamePreviousPath(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/src/old.ts b/src/new.ts",
		"rename from src/old.ts",
		"rename to src/new.ts",
		"--- a/src/old.ts",
		"+++ b/src/new.ts",
		"@@ -1 +1 @@",
		"-old",
		"+new",
	}, "\n")

	inlineDiff := buildExactInlineDiff(diff)
	if inlineDiff == nil || len(inlineDiff.Files) != 1 {
		t.Fatalf("inline diff = %+v, want one file", inlineDiff)
	}
	file := inlineDiff.Files[0]
	if file.Path != "src/new.ts" || file.PreviousPath != "src/old.ts" || file.Kind != "renamed" {
		t.Fatalf("rename metadata = %+v", file)
	}
	if file.PreviewPatch == "" || !strings.Contains(file.PreviewPatch, "rename from src/old.ts") {
		t.Fatalf("preview patch = %q, want rename section", file.PreviewPatch)
	}
}

func TestBuildExactInlineDiffCapsPreviewFiles(t *testing.T) {
	sections := make([]string, 0, inlineDiffPreviewFileCount+2)
	for i := 0; i < inlineDiffPreviewFileCount+2; i++ {
		path := fmt.Sprintf("src/exact-%02d.ts", i)
		sections = append(sections, strings.Join([]string{
			fmt.Sprintf("diff --git a/%s b/%s", path, path),
			fmt.Sprintf("--- a/%s", path),
			fmt.Sprintf("+++ b/%s", path),
			"@@ -1 +1 @@",
			"-old",
			"+new",
		}, "\n"))
	}

	inlineDiff := buildExactInlineDiff(strings.Join(sections, "\n"))
	if inlineDiff == nil {
		t.Fatal("inline diff = nil")
	}
	if len(inlineDiff.Files) != inlineDiffPreviewFileCount {
		t.Fatalf("preview files = %d, want %d", len(inlineDiff.Files), inlineDiffPreviewFileCount)
	}
	if inlineDiff.TotalFiles != inlineDiffPreviewFileCount+2 {
		t.Fatalf("total files = %d, want %d", inlineDiff.TotalFiles, inlineDiffPreviewFileCount+2)
	}
	if inlineDiff.OmittedFiles != 2 || !inlineDiff.FilesTruncated {
		t.Fatalf("truncation metadata = omitted %d truncated %v, want omitted 2 truncated true", inlineDiff.OmittedFiles, inlineDiff.FilesTruncated)
	}
}

func TestExtractFileChangeToolResultPreservesPartialSummaryCounts(t *testing.T) {
	raw := json.RawMessage(`{
		"item": {
			"id": "patch-1",
			"type": "fileChange",
			"changes": [
				{
					"path": "src/exact.ts",
					"kind": {"type": "update", "move_path": null},
					"diff": "@@ -1 +1,2 @@\n const value = 1;\n+const next = 2;"
				},
				{
					"path": "src/summary.ts",
					"kind": {"type": "update", "move_path": null}
				}
			]
		}
	}`)

	meta, diffData, ok := ExtractFileChangeToolResult(raw, "")
	if !ok {
		t.Fatal("expected mixed fileChange extraction to succeed")
	}
	if meta.InlineDiff == nil || meta.InlineDiff.Availability != "summary_only" {
		t.Fatalf("inline diff = %+v, want summary_only", meta.InlineDiff)
	}
	if meta.InlineDiff.Insertions != 1 || meta.InlineDiff.Deletions != 0 {
		t.Fatalf("summary counts = +%d -%d, want +1 -0", meta.InlineDiff.Insertions, meta.InlineDiff.Deletions)
	}
	if meta.Title != "Edited 2 files (+1 -0)" {
		t.Fatalf("title = %q, want %q", meta.Title, "Edited 2 files (+1 -0)")
	}
	if len(diffData) != 0 {
		t.Fatalf("summary-only diff data = %q, want empty", string(diffData))
	}
}

func TestExtractFileChangeToolResultBuildsAddAndDeleteContentPatches(t *testing.T) {
	raw := json.RawMessage(`{
		"item": {
			"id": "patch-1",
			"type": "fileChange",
			"changes": [
				{"path": "src/added.ts", "kind": {"type": "add"}, "diff": "one\ntwo\n"},
				{"path": "src/deleted.ts", "kind": {"type": "delete"}, "diff": "gone\n"}
			]
		}
	}`)

	meta, diffData, ok := ExtractFileChangeToolResult(raw, "")
	if !ok {
		t.Fatal("expected add/delete fileChange extraction to succeed")
	}
	if meta.Title != "Edited 2 files (+2 -1)" {
		t.Fatalf("title = %q, want %q", meta.Title, "Edited 2 files (+2 -1)")
	}
	files := meta.InlineDiff.Files
	if len(files) != 2 {
		t.Fatalf("files = %+v, want 2", files)
	}
	if files[0].Kind != "added" || files[0].Insertions != 2 || files[0].Deletions != 0 {
		t.Fatalf("added file metadata = %+v", files[0])
	}
	if files[1].Kind != "deleted" || files[1].Insertions != 0 || files[1].Deletions != 1 {
		t.Fatalf("deleted file metadata = %+v", files[1])
	}
	diff := string(diffData)
	if !strings.Contains(diff, "+one\n+two") {
		t.Fatalf("expected added content lines, got %q", diff)
	}
	if !strings.Contains(diff, "-gone") {
		t.Fatalf("expected deleted content lines, got %q", diff)
	}
}

func TestFileChangeCompletionWithoutLaunchPreservesTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		itemStatus string
		wantStatus string
	}{
		{name: "failed", itemStatus: "failed", wantStatus: statusErrored},
		{name: "declined", itemStatus: "declined", wantStatus: "declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createToolResultThread(t, st, "t1", t.TempDir())

			meta, err := json.Marshal(map[string]any{
				"toolName":    "file_change",
				"item_status": tc.itemStatus,
				"item": map[string]any{
					"id":   "fc-" + tc.name,
					"type": "fileChange",
					"changes": []map[string]any{{
						"path": "src/app.ts",
						"kind": map[string]any{"type": "update", "move_path": nil},
						"diff": "@@ -1 +1 @@\n-old\n+new",
					}},
				},
			})
			if err != nil {
				t.Fatalf("marshal meta: %v", err)
			}

			if err := router.Handle(provider.ProviderEvent{
				Kind:      provider.EventToolComplete,
				ThreadID:  "t1",
				ItemID:    "fc-" + tc.name,
				ItemType:  "file_change",
				Meta:      meta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("complete: %v", err)
			}

			item, found, err := st.GetThreadItem("t1", "fc-"+tc.name)
			if err != nil {
				t.Fatalf("get item: %v", err)
			}
			if !found {
				t.Fatal("expected no-launch fileChange completion to persist")
			}
			if item.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", item.Status, tc.wantStatus)
			}
			if item.PayloadID == "" {
				t.Fatal("expected rich file-change payload")
			}
		})
	}
}

func TestCommandExecutionToolResultPersistsExactDeletePatch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	payloadID := ToolResultPayloadID("item-command-rm")

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

	if _, err := st.GetPayloadMeta(ToolResultPayloadID("item-command-dependent")); err == nil {
		t.Fatal("expected no payload for dependent command")
	}
	if _, err := st.GetPayloadMeta(ToolResultPayloadID("item-command-failed")); err == nil {
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
