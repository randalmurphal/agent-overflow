// Package triage — Claude-flavored file_change tool_result extractor.
//
// Sibling of tool_result_file_change.go (Codex extractor). The two
// providers describe a file edit with completely different wire shapes
// — Codex sends a normalised `meta.item.changes[]` with per-file
// `path`/`kind`/`diff`, while Claude attaches a per-tool
// `tool_use_result` sibling whose shape varies by tool name. This file
// translates the four Claude file-edit tools' result shapes into the
// same fileChange / ToolResultMeta / unified-diff bytes the Codex
// pipeline produces, then hands off to the existing
// buildInlineDiffFromChanges + buildUnifiedPatch helpers in the
// Codex extractor.
//
// Wire shapes per claude-code-source-code/src/tools/* hunkSchema:
//   - Edit / MultiEdit: `tool_use_result` carries `filePath`,
//     `structuredPatch[]` ({oldStart,oldLines,newStart,newLines,lines}).
//   - Write: `tool_use_result.type` = "create" → `content` (whole file)
//     with empty `structuredPatch`; "update" → `structuredPatch` like
//     Edit. Older shapes may omit `type`; we infer from
//     `structuredPatch` presence.
//   - NotebookEdit: `tool_use_result.notebookPath` plus
//     `original_file` / `updated_file` whole-file strings, no
//     structured patch. We synthesize a unified diff via go-difflib.

package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// extractClaudeFileChangeToolResult translates a Claude EventToolComplete
// Meta into the same (ToolResultMeta, unified-diff bytes) shape the
// Codex extractor returns. Returns ok=false when the wire shape doesn't
// carry enough data to render — caller treats it as a no-op (typical
// on EventToolStart, where tool_use_result hasn't been emitted yet).
//
// `fallbackFilePath` comes from the persisted launch row's
// `Meta.input.file_path` (or `notebook_path`); used only when the
// tool_use_result entry doesn't carry the path itself. Real Claude
// CLIs always populate it, so the fallback is a defensive safety
// net for older versions or partial wire data.
func extractClaudeFileChangeToolResult(rawMeta json.RawMessage, toolName, fallbackFilePath, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	if len(rawMeta) == 0 {
		return ToolResultMeta{}, nil, false
	}

	var envelope struct {
		IsError       bool            `json:"is_error"`
		ToolUseResult json.RawMessage `json:"tool_use_result"`
	}
	if err := json.Unmarshal(rawMeta, &envelope); err != nil {
		return ToolResultMeta{}, nil, false
	}
	// Failed edits surface no diff: the file wasn't written. Drop the
	// row so the timeline shows the tool_call status (errored) without
	// a misleading file_change payload.
	if envelope.IsError {
		return ToolResultMeta{}, nil, false
	}
	if len(envelope.ToolUseResult) == 0 {
		return ToolResultMeta{}, nil, false
	}

	payload, ok := pickClaudeToolUseResultEntry(envelope.ToolUseResult)
	if !ok {
		return ToolResultMeta{}, nil, false
	}

	switch toolName {
	case "Edit", "MultiEdit":
		return claudeStructuredPatchToFileChange(payload, fallbackFilePath, workspaceRoot)
	case "Write":
		return claudeWriteToFileChange(payload, fallbackFilePath, workspaceRoot)
	case "NotebookEdit":
		return claudeNotebookEditToFileChange(payload, fallbackFilePath, workspaceRoot)
	}
	return ToolResultMeta{}, nil, false
}

// pickClaudeToolUseResultEntry handles the three documented shapes for
// `tool_use_result`: bare object (most common), object keyed by
// tool_use_id, or array of entries. Mirrors the parser's
// indexToolUseResults variance handling.
func pickClaudeToolUseResultEntry(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if hasClaudeToolResultFields(obj) {
			return obj, true
		}
		// Object keyed by tool_use_id: pick any entry that looks like
		// a Claude tool_use_result. Order isn't guaranteed by JSON,
		// but the keyed shape is rare and downstream callers only
		// need one entry to extract.
		for _, val := range obj {
			var inner map[string]json.RawMessage
			if json.Unmarshal(val, &inner) == nil && hasClaudeToolResultFields(inner) {
				return inner, true
			}
		}
		return nil, false
	}

	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, entry := range arr {
			var inner map[string]json.RawMessage
			if json.Unmarshal(entry, &inner) == nil && hasClaudeToolResultFields(inner) {
				return inner, true
			}
		}
	}
	return nil, false
}

// hasClaudeToolResultFields recognises the top-level keys produced by
// any of the four file-edit tools. Used to disambiguate "this object
// IS the tool_use_result entry" from "this object KEYS a wrapper map".
func hasClaudeToolResultFields(obj map[string]json.RawMessage) bool {
	for _, key := range []string{"filePath", "notebookPath", "structuredPatch", "content", "original_file", "updated_file", "originalFile", "updatedFile"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// claudeStructuredPatchHunk mirrors the upstream `hunkSchema` shape from
// claude-code-source-code/src/tools/FileEditTool/types.ts. `Lines`
// entries are the body — each starts with `+`, `-`, or ` `. There is
// no `@@` header on the wire; we synthesize one from the line counts.
type claudeStructuredPatchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// claudeStructuredPatchToFileChange handles Edit, MultiEdit, and Write
// (type:"update") — anything that ships hunks via `structuredPatch`.
// Multiple hunks against one file are concatenated into a single
// fileChange entry (Claude tool calls are 1:1 with a file).
func claudeStructuredPatchToFileChange(payload map[string]json.RawMessage, fallbackFilePath, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	filePath := claudeFilePathFromPayload(payload, fallbackFilePath)
	if filePath == "" {
		return ToolResultMeta{}, nil, false
	}
	normalizedPath := normalizeDisplayPath(filePath, workspaceRoot)
	if normalizedPath == "" {
		return ToolResultMeta{}, nil, false
	}

	hunks, ok := claudeReadHunks(payload)
	if !ok || len(hunks) == 0 {
		return ToolResultMeta{}, nil, false
	}

	body := formatClaudeStructuredPatchBody(hunks)
	if body == "" {
		return ToolResultMeta{}, nil, false
	}

	changes := []fileChange{{Path: normalizedPath, Kind: "modified", Diff: body}}
	return finaliseClaudeFileChange(changes)
}

// claudeReadHunks handles the structuredPatch parsing.
func claudeReadHunks(payload map[string]json.RawMessage) ([]claudeStructuredPatchHunk, bool) {
	raw, ok := payload["structuredPatch"]
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var hunks []claudeStructuredPatchHunk
	if err := json.Unmarshal(raw, &hunks); err != nil {
		return nil, false
	}
	return hunks, true
}

// formatClaudeStructuredPatchBody emits one `@@ -O,N +O,N @@` line per
// hunk followed by the hunk's body lines. The result is a header-less
// (no `diff --git` / `---` / `+++`) hunk-only string suitable for the
// `default` branch of buildUnifiedPatch, which prepends file headers.
func formatClaudeStructuredPatchBody(hunks []claudeStructuredPatchHunk) string {
	var sb strings.Builder
	for i, hunk := range hunks {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@", hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines)
		for _, line := range hunk.Lines {
			sb.WriteByte('\n')
			sb.WriteString(line)
		}
	}
	return sb.String()
}

// claudeWriteToFileChange routes Write tool results. Presence of a
// non-empty `structuredPatch` (the "update" path) takes precedence
// and delegates to the shared structured-patch helper. Otherwise it
// treats the payload as a "create" with the file's full content as
// the added body — emitted via the `kind:"added"` branch of
// buildUnifiedPatch which prepends `--- /dev/null`. Older wire shapes
// may omit `type`; structuredPatch presence is the stronger signal.
func claudeWriteToFileChange(payload map[string]json.RawMessage, fallbackFilePath, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	if hunks, ok := claudeReadHunks(payload); ok && len(hunks) > 0 {
		return claudeStructuredPatchToFileChange(payload, fallbackFilePath, workspaceRoot)
	}

	filePath := claudeFilePathFromPayload(payload, fallbackFilePath)
	if filePath == "" {
		return ToolResultMeta{}, nil, false
	}
	normalizedPath := normalizeDisplayPath(filePath, workspaceRoot)
	if normalizedPath == "" {
		return ToolResultMeta{}, nil, false
	}

	content := rawString(payload, "content")
	typ := rawString(payload, "type")
	if typ == "create" || (typ == "" && content != "") {
		changes := []fileChange{{Path: normalizedPath, Kind: "added", Diff: content}}
		return finaliseClaudeFileChange(changes)
	}
	return ToolResultMeta{}, nil, false
}

// notebookEditDiffInputCap bounds the total input size fed to
// go-difflib.GetUnifiedDiffString. The underlying SequenceMatcher is
// O(N²) expected, O(N³) worst case — a multi-MB notebook on the
// triage hot path would stall the per-thread router. Above this cap
// we fall through to the summary-only path (the per-turn EventDiff
// upgrade can still attach a real diff from git later). 256 KiB
// keeps worst-case latency comfortably under ~50ms on typical
// hardware. Tunable.
const notebookEditDiffInputCap = 256 * 1024

// claudeNotebookEditToFileChange synthesises a unified diff between
// `original_file` and `updated_file`. NotebookEdit is the one Claude
// file-edit tool that doesn't ship a structuredPatch — the upstream
// schema (NotebookEditTool.ts:60-85) carries the whole-file before/after
// instead. go-difflib produces a deterministic, well-tested unified
// diff with 3 lines of context (matching the `CONTEXT_LINES = 3`
// constant in claude-code-source-code/src/utils/diff.ts:9).
func claudeNotebookEditToFileChange(payload map[string]json.RawMessage, fallbackFilePath, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	filePath := claudeNotebookPathFromPayload(payload, fallbackFilePath)
	if filePath == "" {
		return ToolResultMeta{}, nil, false
	}
	normalizedPath := normalizeDisplayPath(filePath, workspaceRoot)
	if normalizedPath == "" {
		return ToolResultMeta{}, nil, false
	}

	original := claudeNotebookContentField(payload, "original_file", "originalFile")
	updated := claudeNotebookContentField(payload, "updated_file", "updatedFile")

	// Cap input before invoking difflib (see notebookEditDiffInputCap).
	// Identical-content fallback also lands here — the row still
	// appears with the path; the per-turn diff upgrade can fill it.
	if len(original)+len(updated) > notebookEditDiffInputCap {
		return claudeNotebookSummaryOnly(normalizedPath)
	}

	udiff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(original),
		B:        difflib.SplitLines(updated),
		FromFile: "a/" + normalizedPath,
		ToFile:   "b/" + normalizedPath,
		Context:  3,
	}
	patch, err := difflib.GetUnifiedDiffString(udiff)
	if err != nil {
		// Library failure — drop the row rather than persist a stub.
		// difflib doesn't actually return errors in practice (the
		// signature is a future-proofing artifact), so this is
		// defense-in-depth.
		return ToolResultMeta{}, nil, false
	}
	if strings.TrimSpace(patch) == "" {
		// Identical content. The row still appears with the path; a
		// later EventDiff can upgrade it from git.
		return claudeNotebookSummaryOnly(normalizedPath)
	}

	// Prepend a `diff --git` header so buildUnifiedPatch's default
	// branch passes the body through ExtractDiffMeta unchanged.
	fullPatch := fmt.Sprintf("diff --git a/%s b/%s\n%s", normalizedPath, normalizedPath, patch)
	changes := []fileChange{{Path: normalizedPath, Kind: "modified", Diff: fullPatch}}
	return finaliseClaudeFileChange(changes)
}

func claudeNotebookSummaryOnly(normalizedPath string) (ToolResultMeta, []byte, bool) {
	changes := []fileChange{{Path: normalizedPath, Kind: "modified", Diff: ""}}
	return finaliseClaudeFileChange(changes)
}

// finaliseClaudeFileChange runs the shared Codex pipeline and stamps
// ItemType / Title / Preview onto the resulting meta the same way
// extractFileChangeToolResult does for Codex.
func finaliseClaudeFileChange(changes []fileChange) (ToolResultMeta, []byte, bool) {
	if len(changes) == 0 {
		return ToolResultMeta{}, nil, false
	}
	inlineDiff, unifiedDiff := buildInlineDiffFromChanges(changes)
	if inlineDiff == nil {
		return ToolResultMeta{}, nil, false
	}
	meta := ToolResultMeta{
		ItemType:   "file_change",
		InlineDiff: inlineDiff,
	}
	meta.Title = fileChangeTitle(inlineDiff)
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

func claudeFilePathFromPayload(payload map[string]json.RawMessage, fallback string) string {
	if path := rawString(payload, "filePath"); path != "" {
		return path
	}
	if path := rawString(payload, "file_path"); path != "" {
		return path
	}
	return fallback
}

func claudeNotebookPathFromPayload(payload map[string]json.RawMessage, fallback string) string {
	if path := rawString(payload, "notebookPath"); path != "" {
		return path
	}
	if path := rawString(payload, "notebook_path"); path != "" {
		return path
	}
	return claudeFilePathFromPayload(payload, fallback)
}

// claudeNotebookContentField reads a whole-file content field across
// snake_case / camelCase variants. Returns "" if neither key is set or
// the value isn't a string.
func claudeNotebookContentField(payload map[string]json.RawMessage, primary, secondary string) string {
	if v := rawString(payload, primary); v != "" {
		return v
	}
	return rawString(payload, secondary)
}

// extractClaudeLaunchFilePath reads the file path the Claude tool
// committed to write at start time. Used as a fallback when
// tool_use_result doesn't carry the path. Sources are the persisted
// `items.meta` shape produced by marshalToolMeta in
// internal/provider/claude/parse_assistant.go: `{"toolName":"Edit","input":{"file_path":"..."}}`.
func extractClaudeLaunchFilePath(metaJSON string) string {
	if metaJSON == "" {
		return ""
	}
	var envelope struct {
		Input struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &envelope); err != nil {
		return ""
	}
	if envelope.Input.FilePath != "" {
		return envelope.Input.FilePath
	}
	return envelope.Input.NotebookPath
}
