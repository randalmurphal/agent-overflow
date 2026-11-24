package triage

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// claudeMeta builds the EventToolComplete Meta JSON shape produced by
// the Claude parser's appendToolResultCompletion: an envelope with
// `tool_result` (the block) and `tool_use_result` (the structured
// sibling, where the file_path / structuredPatch / content live).
func claudeMeta(toolUseResult any) json.RawMessage {
	turRaw, err := json.Marshal(toolUseResult)
	if err != nil {
		panic(err)
	}
	envelope := map[string]any{
		"is_error":        false,
		"tool_result":     map[string]any{"type": "tool_result", "tool_use_id": "tu_test"},
		"tool_use_result": json.RawMessage(turRaw),
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return out
}

func TestExtractClaudeEdit_SingleHunk(t *testing.T) {
	meta := claudeMeta(map[string]any{
		"filePath": "src/app.ts",
		"structuredPatch": []map[string]any{{
			"oldStart": 1,
			"oldLines": 2,
			"newStart": 1,
			"newLines": 3,
			"lines": []string{
				" const value = 1;",
				"+const next = 2;",
				" const last = 3;",
			},
		}},
	})

	result, diffData, ok := ExtractClaudeFileChangeToolResult(meta, "Edit", "", "")
	if !ok {
		t.Fatal("expected Edit extraction to succeed")
	}
	if result.InlineDiff == nil || result.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", result.InlineDiff)
	}
	if len(result.InlineDiff.Files) != 1 || result.InlineDiff.Files[0].Path != "src/app.ts" {
		t.Fatalf("files = %+v, want one entry for src/app.ts", result.InlineDiff.Files)
	}
	if result.InlineDiff.Files[0].Insertions != 1 || result.InlineDiff.Files[0].Deletions != 0 {
		t.Fatalf("counts = +%d -%d, want +1 -0", result.InlineDiff.Files[0].Insertions, result.InlineDiff.Files[0].Deletions)
	}
	patch := string(diffData)
	if !strings.Contains(patch, "diff --git a/src/app.ts b/src/app.ts") {
		t.Fatalf("expected diff --git header, got %q", patch)
	}
	if !strings.Contains(patch, "@@ -1,2 +1,3 @@") {
		t.Fatalf("expected synthesized hunk header, got %q", patch)
	}
	if !strings.Contains(patch, "+const next = 2;") {
		t.Fatalf("expected hunk body, got %q", patch)
	}
}

func TestExtractClaudeMultiEdit_MultiHunk(t *testing.T) {
	meta := claudeMeta(map[string]any{
		"filePath": "src/app.ts",
		"structuredPatch": []map[string]any{
			{
				"oldStart": 1,
				"oldLines": 1,
				"newStart": 1,
				"newLines": 2,
				"lines": []string{
					" first;",
					"+inserted_first;",
				},
			},
			{
				"oldStart": 10,
				"oldLines": 2,
				"newStart": 11,
				"newLines": 1,
				"lines": []string{
					" tenth;",
					"-eleventh;",
				},
			},
		},
	})

	result, diffData, ok := ExtractClaudeFileChangeToolResult(meta, "MultiEdit", "", "")
	if !ok {
		t.Fatal("expected MultiEdit extraction to succeed")
	}
	if result.InlineDiff == nil || len(result.InlineDiff.Files) != 1 {
		t.Fatalf("expected one file entry, got %+v", result.InlineDiff)
	}
	if result.InlineDiff.Files[0].Insertions != 1 || result.InlineDiff.Files[0].Deletions != 1 {
		t.Fatalf("counts = +%d -%d, want +1 -1", result.InlineDiff.Files[0].Insertions, result.InlineDiff.Files[0].Deletions)
	}
	patch := string(diffData)
	if !strings.Contains(patch, "@@ -1,1 +1,2 @@") || !strings.Contains(patch, "@@ -10,2 +11,1 @@") {
		t.Fatalf("expected both hunk headers, got %q", patch)
	}
}

func TestExtractClaudeWrite_Create(t *testing.T) {
	meta := claudeMeta(map[string]any{
		"type":            "create",
		"filePath":        "src/new.ts",
		"content":         "export const fresh = true;\n",
		"structuredPatch": []any{},
	})

	result, diffData, ok := ExtractClaudeFileChangeToolResult(meta, "Write", "", "")
	if !ok {
		t.Fatal("expected Write create extraction to succeed")
	}
	if result.InlineDiff == nil || len(result.InlineDiff.Files) != 1 {
		t.Fatalf("expected one file entry, got %+v", result.InlineDiff)
	}
	file := result.InlineDiff.Files[0]
	if file.Kind != "added" {
		t.Fatalf("kind = %q, want added", file.Kind)
	}
	if file.Insertions != 1 || file.Deletions != 0 {
		t.Fatalf("counts = +%d -%d, want +1 -0", file.Insertions, file.Deletions)
	}
	patch := string(diffData)
	if !strings.Contains(patch, "new file mode 100644") {
		t.Fatalf("expected new file header, got %q", patch)
	}
	if !strings.Contains(patch, "--- /dev/null") {
		t.Fatalf("expected /dev/null header, got %q", patch)
	}
	if !strings.Contains(patch, "+export const fresh = true;") {
		t.Fatalf("expected added content line, got %q", patch)
	}
}

func TestExtractClaudeWrite_Update(t *testing.T) {
	// Write update ships structuredPatch like Edit.
	meta := claudeMeta(map[string]any{
		"type":     "update",
		"filePath": "src/existing.ts",
		"structuredPatch": []map[string]any{{
			"oldStart": 1,
			"oldLines": 1,
			"newStart": 1,
			"newLines": 1,
			"lines": []string{
				"-old;",
				"+new;",
			},
		}},
	})

	result, _, ok := ExtractClaudeFileChangeToolResult(meta, "Write", "", "")
	if !ok {
		t.Fatal("expected Write update extraction to succeed")
	}
	if result.InlineDiff.Files[0].Kind != "modified" {
		t.Fatalf("kind = %q, want modified", result.InlineDiff.Files[0].Kind)
	}
	if result.InlineDiff.Files[0].Insertions != 1 || result.InlineDiff.Files[0].Deletions != 1 {
		t.Fatalf("counts = +%d -%d, want +1 -1", result.InlineDiff.Files[0].Insertions, result.InlineDiff.Files[0].Deletions)
	}
}

func TestExtractClaudeNotebookEdit_UnifiedDiffSynthesis(t *testing.T) {
	original := strings.Join([]string{"line1", "line2", "line3", "line4"}, "\n") + "\n"
	updated := strings.Join([]string{"line1", "line2 changed", "line3", "line4"}, "\n") + "\n"

	meta := claudeMeta(map[string]any{
		"notebookPath":  "notebooks/example.ipynb",
		"original_file": original,
		"updated_file":  updated,
	})

	result, diffData, ok := ExtractClaudeFileChangeToolResult(meta, "NotebookEdit", "", "")
	if !ok {
		t.Fatal("expected NotebookEdit extraction to succeed")
	}
	if result.InlineDiff == nil || len(result.InlineDiff.Files) != 1 {
		t.Fatalf("expected one file entry, got %+v", result.InlineDiff)
	}
	if result.InlineDiff.Files[0].Path != "notebooks/example.ipynb" {
		t.Fatalf("path = %q, want notebooks/example.ipynb", result.InlineDiff.Files[0].Path)
	}
	if result.InlineDiff.Files[0].Insertions != 1 || result.InlineDiff.Files[0].Deletions != 1 {
		t.Fatalf("counts = +%d -%d, want +1 -1", result.InlineDiff.Files[0].Insertions, result.InlineDiff.Files[0].Deletions)
	}
	patch := string(diffData)
	if !strings.Contains(patch, "diff --git a/notebooks/example.ipynb b/notebooks/example.ipynb") {
		t.Fatalf("expected diff --git header, got %q", patch)
	}
	if !strings.Contains(patch, "-line2") || !strings.Contains(patch, "+line2 changed") {
		t.Fatalf("expected change lines in synthesized diff, got %q", patch)
	}
}

// A NotebookEdit with no diff to show still surfaces its row, as
// summary_only with no diff bytes: identical content has no diff, and
// input larger than notebookEditDiffInputCap must short-circuit rather
// than invoke difflib (O(N²) expected, it would stall the per-thread
// router on multi-MB notebooks).
func TestExtractClaudeNotebookEdit_SummaryOnly(t *testing.T) {
	for _, tc := range []struct {
		name, path, original, updated string
	}{
		{"identical content", "notebooks/same.ipynb", "unchanged\n", "unchanged\n"},
		// 200 KiB each → 400 KiB total, above the 256 KiB cap.
		{"oversized input", "notebooks/big.ipynb", strings.Repeat("a\n", 100*1024), strings.Repeat("b\n", 100*1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := claudeMeta(map[string]any{
				"notebookPath":  tc.path,
				"original_file": tc.original,
				"updated_file":  tc.updated,
			})
			result, diffData, ok := ExtractClaudeFileChangeToolResult(meta, "NotebookEdit", "", "")
			if !ok {
				t.Fatal("expected NotebookEdit to surface as summary_only, not drop")
			}
			if result.InlineDiff.Availability != "summary_only" {
				t.Fatalf("availability = %q, want summary_only", result.InlineDiff.Availability)
			}
			if len(diffData) != 0 {
				t.Fatalf("expected no diff bytes, got %d bytes", len(diffData))
			}
		})
	}
}

// Pins the polarity of the is_error short-circuit: a successful-looking
// payload must still be dropped when the wire said the tool failed.
func TestExtractClaude_IsErrorDropsSuccessfulShape(t *testing.T) {
	envelope := map[string]any{
		"is_error":    true,
		"tool_result": map[string]any{"type": "tool_result", "tool_use_id": "tu_failed"},
		"tool_use_result": map[string]any{
			"filePath": "src/app.ts",
			"structuredPatch": []map[string]any{{
				"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
				"lines": []string{"-old;", "+new;"},
			}},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, ok := ExtractClaudeFileChangeToolResult(raw, "Edit", "", ""); ok {
		t.Fatal("is_error=true must drop even when structuredPatch would otherwise extract cleanly")
	}
}

// tool_use_result wire shape variance — Claude's parser
// (parse_user.go indexToolUseResults) accepts bare object, object
// keyed by tool_use_id, or array. The extractor's
// pickClaudeToolUseResultEntry handles all three; the bare object is
// claudeMeta's shape, the wrappers are covered here.
func TestExtractClaude_ToolUseResultWrapperShapes(t *testing.T) {
	entry := func(path string) map[string]any {
		return map[string]any{
			"filePath": path,
			"structuredPatch": []map[string]any{{
				"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
				"lines": []string{"-old;", "+new;"},
			}},
		}
	}
	for _, tc := range []struct {
		name, toolUseID string
		toolUseResult   any
		wantPath        string
	}{
		{"array", "tu_array", []any{entry("src/array.ts")}, "src/array.ts"},
		{"keyed by tool_use_id", "tu_keyed", map[string]any{"tu_keyed": entry("src/keyed.ts")}, "src/keyed.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"is_error":        false,
				"tool_result":     map[string]any{"type": "tool_result", "tool_use_id": tc.toolUseID},
				"tool_use_result": tc.toolUseResult,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			result, _, ok := ExtractClaudeFileChangeToolResult(raw, "Edit", "", "")
			if !ok {
				t.Fatalf("expected %s tool_use_result to extract", tc.name)
			}
			if result.InlineDiff.Files[0].Path != tc.wantPath {
				t.Fatalf("path = %q, want %s", result.InlineDiff.Files[0].Path, tc.wantPath)
			}
		})
	}
}

// An Edit's displayed path: a workspace path is made relative, a path
// outside the workspace stays absolute, and a tool_use_result without
// filePath falls back to the launch row's input.file_path.
// Regression (outside workspace): the original Claude extractor
// inherited the strict `normalizeWorkspaceRelativePath` from the Codex
// path, which silently rejected anything outside the workspace, so an
// Edit on `/tmp/foo.txt` produced no diff in the UI.
func TestExtractClaude_EditPathResolution(t *testing.T) {
	workspace := t.TempDir()
	hunk := []map[string]any{{
		"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
		"lines": []string{"-old;", "+new;"},
	}}
	for _, tc := range []struct {
		name                    string
		toolUseResult           map[string]any
		fallbackPath, workspace string
		wantPath                string
	}{
		{
			name:          "absolute path in workspace normalized",
			toolUseResult: map[string]any{"filePath": filepath.Join(workspace, "src", "deep.ts"), "structuredPatch": hunk},
			workspace:     workspace,
			wantPath:      "src/deep.ts",
		},
		{
			name:          "absolute path outside workspace preserved",
			toolUseResult: map[string]any{"filePath": "/tmp/diff-test-scratch.txt", "structuredPatch": hunk},
			workspace:     workspace,
			wantPath:      "/tmp/diff-test-scratch.txt",
		},
		{
			name:          "fallback path from launch row",
			toolUseResult: map[string]any{"structuredPatch": hunk},
			fallbackPath:  "src/from-launch.ts",
			wantPath:      "src/from-launch.ts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, ok := ExtractClaudeFileChangeToolResult(claudeMeta(tc.toolUseResult), "Edit", tc.fallbackPath, tc.workspace)
			if !ok {
				t.Fatal("expected Edit extraction to succeed")
			}
			if got := result.InlineDiff.Files[0].Path; got != tc.wantPath {
				t.Fatalf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

func TestExtractClaude_NoToolUseResultIsNoop(t *testing.T) {
	// EventToolStart shape: meta has tool_result-like keys but no
	// tool_use_result yet. Extractor should silently return false
	// so the dispatcher in persistFileChangeToolResult is a no-op.
	envelope := map[string]any{
		"is_error":    false,
		"tool_result": map[string]any{"type": "tool_use", "name": "Edit"},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, ok := ExtractClaudeFileChangeToolResult(raw, "Edit", "", ""); ok {
		t.Fatal("expected no-tool_use_result meta to skip extraction")
	}
}

func TestExtractClaude_UnknownToolNameRejected(t *testing.T) {
	meta := claudeMeta(map[string]any{
		"filePath": "src/app.ts",
	})
	if _, _, ok := ExtractClaudeFileChangeToolResult(meta, "Bash", "", ""); ok {
		t.Fatal("expected non-file-change tool name to skip extraction")
	}
}

// Pins the NotebookEdit identical-content → summary_only → upgrade
// path. Without this, a parser change that broke the upgrade pipeline
// for Claude rows wouldn't be caught (the existing
// TestFileChangeToolResultUpgradesFromTurnDiff exercises Codex shape).
func TestNotebookEditSummaryOnlyUpgradesFromTurnDiff(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t-nb", workspace)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-nb",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	notebookPath := filepath.Join(workspace, "notebooks", "ex.ipynb")
	startMeta, err := json.Marshal(map[string]any{
		"toolName": "NotebookEdit",
		"input":    map[string]any{"notebook_path": notebookPath},
	})
	if err != nil {
		t.Fatalf("marshal start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t-nb",
		ItemID:    "tu_nb_1",
		ItemType:  "NotebookEdit",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Identical original/updated → summary_only row (the realistic
	// pre-upgrade state when Claude reports cell-level changes that
	// don't show up in the whole-file before/after).
	completeMeta := claudeMeta(map[string]any{
		"notebookPath":  notebookPath,
		"original_file": "same\n",
		"updated_file":  "same\n",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t-nb",
		ItemID:    "tu_nb_1",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	payloadID := ToolResultPayloadID("tu_nb_1")
	pre := readToolResultMeta(t, st, "t-nb", payloadID)
	if pre.InlineDiff == nil || pre.InlineDiff.Availability != "summary_only" {
		t.Fatalf("expected summary_only pre-upgrade, got %+v", pre.InlineDiff)
	}

	turnDiff := strings.Join([]string{
		"diff --git a/notebooks/ex.ipynb b/notebooks/ex.ipynb",
		"--- a/notebooks/ex.ipynb",
		"+++ b/notebooks/ex.ipynb",
		"@@ -1 +1,2 @@",
		" same",
		"+changed-by-git",
	}, "\n")
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t-nb",
		Content:   turnDiff,
		Meta:      json.RawMessage(`{"upgrade_only":true,"source":"turn/diff/updated"}`),
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn diff: %v", err)
	}

	post := readToolResultMeta(t, st, "t-nb", payloadID)
	if post.InlineDiff == nil || post.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("expected exact_patch after upgrade, got %+v", post.InlineDiff)
	}
}

// End-to-end: raw Claude NDJSON → parser.ParseLine → router.Handle →
// persisted payload. Reproduces the user-reported flow exactly: an
// Edit on /tmp/diff-test.txt with structuredPatch shipped via
// bare-object `tool_use_result` (the shape FileEditTool actually
// emits, NOT the indexed-array shape). Catches any parser-triage
// composition bug the unit tests miss.
func TestEndToEndClaudeEditProducesDiffPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t-e2e", workspace)

	parser := claude.NewParser()

	// 1. system.init — gives the parser a session id.
	if _, err := parser.ParseLine("t-e2e", []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-sonnet-4-6","tools":["Edit"],"cwd":"`+workspace+`"}`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 2. assistant tool_use — Edit on /tmp/diff-test.txt.
	assistant := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tu_edit_e2e","name":"Edit","input":{"file_path":"/tmp/diff-test.txt","old_string":"old line","new_string":"new line"}}]}}`)
	startEvents, err := parser.ParseLine("t-e2e", assistant)
	if err != nil {
		t.Fatalf("assistant parse: %v", err)
	}
	// EventTurnStart auto-fires once per session; route everything
	// the parser emits.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-e2e",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	for _, evt := range startEvents {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("route start event %s: %v", evt.Kind, err)
		}
	}

	// 3. user tool_result — bare-object tool_use_result with
	// structuredPatch (the FileEditTool wire shape).
	user := []byte(`{"type":"user","tool_use_result":{"filePath":"/tmp/diff-test.txt","oldString":"old line","newString":"new line","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-old line","+new line"]}]},"message":{"role":"user","content":[{"tool_use_id":"tu_edit_e2e","type":"tool_result","content":"The file /tmp/diff-test.txt has been updated successfully."}]}}`)
	completeEvents, err := parser.ParseLine("t-e2e", user)
	if err != nil {
		t.Fatalf("user parse: %v", err)
	}
	if len(completeEvents) != 1 {
		t.Fatalf("expected 1 EventToolComplete, got %d events: %+v", len(completeEvents), completeEvents)
	}
	if completeEvents[0].Kind != provider.EventToolComplete {
		t.Fatalf("expected EventToolComplete, got %s", completeEvents[0].Kind)
	}
	for _, evt := range completeEvents {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("route complete event: %v", err)
		}
	}

	// 4. Verify the payload landed with the unified-diff bytes.
	payloadID := ToolResultPayloadID("tu_edit_e2e")
	pm, err := st.GetPayloadMeta("t-e2e", payloadID)
	if err != nil {
		t.Fatalf("payload meta: %v (no payload was written — parser→triage composition is broken)", err)
	}
	var resultMeta ToolResultMeta
	if err := json.Unmarshal([]byte(pm.Meta), &resultMeta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resultMeta.InlineDiff == nil {
		t.Fatalf("inlineDiff is nil — extractor returned no inline diff")
	}
	if resultMeta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("availability = %q, want exact_patch (got: %+v)", resultMeta.InlineDiff.Availability, resultMeta.InlineDiff)
	}
	if len(resultMeta.InlineDiff.Files) != 1 {
		t.Fatalf("files = %+v, want 1", resultMeta.InlineDiff.Files)
	}
	if resultMeta.InlineDiff.Files[0].Path != "/tmp/diff-test.txt" {
		t.Fatalf("path = %q, want /tmp/diff-test.txt", resultMeta.InlineDiff.Files[0].Path)
	}

	// 5. The unified-diff bytes must contain the change.
	data, err := st.GetPayloadData("t-e2e", payloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	patch := string(data)
	if !strings.Contains(patch, "+new line") || !strings.Contains(patch, "-old line") {
		t.Fatalf("expected hunk lines in payload data, got: %q", patch)
	}
	if !strings.Contains(patch, "/tmp/diff-test.txt") {
		t.Fatalf("expected file path in unified diff, got: %q", patch)
	}
}

func TestPersistClaudeFileChangeToolResult_RoutesViaLaunchRowToolName(t *testing.T) {
	// Integration: simulates the wire ordering — launch row created
	// at EventToolStart with toolName="Edit", complete event arrives
	// with empty ItemType but rich tool_use_result. Triage must
	// resolve the tool name from the persisted launch row and
	// dispatch to the Claude extractor.
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)

	// Open turn so the lifecycle row attaches correctly.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Claude EventToolStart for an Edit. ItemType set to "Edit",
	// Meta carries input.file_path per marshalToolMeta.
	startMeta, err := json.Marshal(map[string]any{
		"toolName": "Edit",
		"input": map[string]any{
			"file_path":  filepath.Join(workspace, "src", "app.ts"),
			"old_string": "old",
			"new_string": "new",
		},
	})
	if err != nil {
		t.Fatalf("marshal start meta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "tu_edit_1",
		ItemType:  "Edit",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// EventToolComplete arrives with empty ItemType (Claude wire
	// shape). tool_use_result carries the structuredPatch.
	completeMeta := claudeMeta(map[string]any{
		"filePath": filepath.Join(workspace, "src", "app.ts"),
		"structuredPatch": []map[string]any{{
			"oldStart": 1,
			"oldLines": 1,
			"newStart": 1,
			"newLines": 1,
			"lines":    []string{"-old;", "+new;"},
		}},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "tu_edit_1",
		ItemType:  "", // empty per Claude appendToolResultCompletion
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	// Verify the tool_result payload landed and carries the rich diff.
	payloadID := ToolResultPayloadID("tu_edit_1")
	pm, err := st.GetPayloadMeta("t1", payloadID)
	if err != nil {
		t.Fatalf("payload meta: %v", err)
	}
	var resultMeta ToolResultMeta
	if err := json.Unmarshal([]byte(pm.Meta), &resultMeta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resultMeta.InlineDiff == nil || resultMeta.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("inline diff = %+v, want exact_patch", resultMeta.InlineDiff)
	}
	if resultMeta.InlineDiff.Files[0].Path != "src/app.ts" {
		t.Fatalf("path = %q, want workspace-relative src/app.ts", resultMeta.InlineDiff.Files[0].Path)
	}

	data, err := st.GetPayloadData("t1", payloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if !strings.Contains(string(data), "+new;") || !strings.Contains(string(data), "-old;") {
		t.Fatalf("expected hunk body in payload, got %q", string(data))
	}
}
