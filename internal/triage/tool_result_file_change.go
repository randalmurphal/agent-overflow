package triage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/stringsx"
)

const (
	toolResultPayloadKind = "tool_result"
	toolResultItemKind    = "tool_result"
)

type ToolResultMeta struct {
	ItemType   string          `json:"itemType"`
	Title      string          `json:"title"`
	Detail     string          `json:"detail,omitempty"`
	Preview    string          `json:"preview,omitempty"`
	InlineDiff *ToolInlineDiff `json:"inlineDiff,omitempty"`
}

type ToolInlineDiff struct {
	Availability string               `json:"availability"`
	Files        []ToolInlineDiffFile `json:"files"`
	Insertions   int                  `json:"insertions,omitempty"`
	Deletions    int                  `json:"deletions,omitempty"`
}

type ToolInlineDiffFile struct {
	Path       string `json:"path"`
	Kind       string `json:"kind,omitempty"`
	Insertions int    `json:"insertions,omitempty"`
	Deletions  int    `json:"deletions,omitempty"`
}

func (r *Router) persistFileChangeToolResult(evt provider.ProviderEvent) error {
	if evt.ItemType != "file_change" || evt.ItemID == "" || len(evt.Meta) == 0 {
		return nil
	}

	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("lookup thread for tool result: %w", err)
	}

	meta, diffData, ok := extractFileChangeToolResult(evt.Meta, thread.WorkspacePath)
	if !ok {
		return nil
	}
	return r.persistToolResult(evt, meta, diffData)
}

func (r *Router) mergeToolResultPayload(payloadID string, next ToolResultMeta, nextDiff []byte) (ToolResultMeta, []byte) {
	pm, err := r.store.GetPayloadMeta(payloadID)
	if err != nil || pm.Kind != toolResultPayloadKind {
		return next, nextDiff
	}

	var existing ToolResultMeta
	if json.Unmarshal([]byte(pm.Meta), &existing) != nil {
		return next, nextDiff
	}
	if !hasExactToolInlineDiff(existing.InlineDiff) || hasExactToolInlineDiff(next.InlineDiff) {
		return next, nextDiff
	}

	data, dataErr := r.store.GetPayloadData(payloadID)
	if dataErr != nil {
		return existing, nextDiff
	}
	return existing, data
}

func extractFileChangeToolResult(raw json.RawMessage, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return ToolResultMeta{}, nil, false
	}

	itemRaw, ok := payload["item"]
	if !ok {
		return ToolResultMeta{}, nil, false
	}

	var item map[string]json.RawMessage
	if json.Unmarshal(itemRaw, &item) != nil {
		return ToolResultMeta{}, nil, false
	}

	changes := extractFileChanges(item, workspaceRoot)
	if len(changes) == 0 {
		return ToolResultMeta{}, nil, false
	}

	meta := ToolResultMeta{
		ItemType: "file_change",
		Title:    stringsx.FirstNonEmptyTrimmed(rawString(item, "title"), "File change"),
		Detail:   rawString(item, "detail"),
	}
	inlineDiff, unifiedDiff := buildInlineDiffFromChanges(changes)
	if inlineDiff != nil {
		meta.InlineDiff = inlineDiff
	}
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

type fileChange struct {
	Path string
	Kind string
	Diff string
}

func extractFileChanges(item map[string]json.RawMessage, workspaceRoot string) []fileChange {
	dataRaw, ok := item["data"]
	if !ok {
		return nil
	}

	var data map[string]json.RawMessage
	if json.Unmarshal(dataRaw, &data) != nil {
		return nil
	}

	itemDataRaw, ok := data["item"]
	if !ok {
		return nil
	}

	var itemData map[string]json.RawMessage
	if json.Unmarshal(itemDataRaw, &itemData) != nil {
		return nil
	}

	changesRaw, ok := itemData["changes"]
	if !ok {
		return nil
	}

	var rawChanges []map[string]json.RawMessage
	if json.Unmarshal(changesRaw, &rawChanges) != nil {
		return nil
	}

	changes := make([]fileChange, 0, len(rawChanges))
	for _, change := range rawChanges {
		path := normalizeToolPath(rawString(change, "path"), workspaceRoot)
		if path == "" {
			continue
		}
		changes = append(changes, fileChange{
			Path: path,
			Kind: normalizeChangeKind(change["kind"]),
			Diff: rawString(change, "diff"),
		})
	}
	return changes
}

func buildInlineDiffFromChanges(changes []fileChange) (*ToolInlineDiff, string) {
	files := make([]ToolInlineDiffFile, 0, len(changes))
	patches := make([]string, 0, len(changes))
	exact := true
	totalInsertions := 0
	totalDeletions := 0

	for _, change := range changes {
		file := ToolInlineDiffFile{Path: change.Path, Kind: change.Kind}
		if patch, ok := buildUnifiedPatch(change); ok {
			diffMeta := ExtractDiffMeta(patch)
			file.Kind = diffMeta.ChangeKind
			file.Insertions = diffMeta.Insertions
			file.Deletions = diffMeta.Deletions
			totalInsertions += diffMeta.Insertions
			totalDeletions += diffMeta.Deletions
			patches = append(patches, patch)
		} else {
			exact = false
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		return nil, ""
	}
	if !exact || len(patches) != len(files) {
		return &ToolInlineDiff{
			Availability: "summary_only",
			Files:        files,
		}, ""
	}

	return &ToolInlineDiff{
			Availability: "exact_patch",
			Files:        files,
			Insertions:   totalInsertions,
			Deletions:    totalDeletions,
		},
		strings.Join(patches, "\n")
}

func buildUnifiedPatch(change fileChange) (string, bool) {
	hunk := strings.TrimSpace(change.Diff)
	if hunk == "" {
		return "", false
	}

	var header []string
	switch change.Kind {
	case "added":
		header = []string{
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			"new file mode 100644",
			"--- /dev/null",
			fmt.Sprintf("+++ b/%s", change.Path),
		}
	case "deleted":
		header = []string{
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			"deleted file mode 100644",
			fmt.Sprintf("--- a/%s", change.Path),
			"+++ /dev/null",
		}
	case "renamed":
		return "", false
	default:
		header = []string{
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			fmt.Sprintf("--- a/%s", change.Path),
			fmt.Sprintf("+++ b/%s", change.Path),
		}
	}

	return strings.Join(append(header, hunk), "\n"), true
}

func normalizeChangeKind(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case "added", "create":
			return "added"
		case "deleted", "remove":
			return "deleted"
		case "renamed", "move":
			return "renamed"
		default:
			return "modified"
		}
	}

	var object struct {
		Type     string `json:"type"`
		MovePath string `json:"move_path"`
	}
	if json.Unmarshal(raw, &object) == nil {
		switch object.Type {
		case "create":
			return "added"
		case "delete":
			return "deleted"
		case "move":
			return "renamed"
		default:
			return "modified"
		}
	}
	return "modified"
}

func rawString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func summarizeToolResult(meta ToolResultMeta) string {
	switch {
	case meta.Detail != "":
		return meta.Detail
	case meta.Preview != "":
		return meta.Preview
	case meta.Title != "":
		return meta.Title
	default:
		return "Tool result"
	}
}

func toolPreview(meta ToolResultMeta) string {
	if meta.Detail != "" {
		return meta.Detail
	}
	if meta.InlineDiff == nil || len(meta.InlineDiff.Files) == 0 {
		return meta.Title
	}
	if len(meta.InlineDiff.Files) == 1 {
		return meta.InlineDiff.Files[0].Path
	}
	return fmt.Sprintf("%d files changed", len(meta.InlineDiff.Files))
}

func toolResultPayloadID(itemID string) string {
	return "tool-result:" + itemID
}

func hasExactToolInlineDiff(inlineDiff *ToolInlineDiff) bool {
	return inlineDiff != nil && inlineDiff.Availability == "exact_patch"
}

func normalizeToolPath(rawPath, workspaceRoot string) string {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return ""
	}

	cleanPath := filepath.Clean(rawPath)
	if workspaceRoot != "" && filepath.IsAbs(cleanPath) {
		cleanRoot := filepath.Clean(workspaceRoot)
		rel, err := filepath.Rel(cleanRoot, cleanPath)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cleanPath = rel
		}
	}
	return filepath.ToSlash(cleanPath)
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

