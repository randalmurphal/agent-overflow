package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
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
	Path         string `json:"path"`
	PreviousPath string `json:"previousPath,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
}

func (r *Router) persistFileChangeToolResult(evt provider.ProviderEvent) error {
	if !isFileChangeItemType(evt.ItemType) || evt.ItemID == "" || len(evt.Meta) == 0 {
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
	}
	inlineDiff, unifiedDiff := buildInlineDiffFromChanges(changes)
	if inlineDiff != nil {
		meta.InlineDiff = inlineDiff
	}
	meta.Title = fileChangeTitle(inlineDiff)
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

type fileChange struct {
	Path         string
	PreviousPath string
	Kind         string
	Diff         string
}

type fileChangePatch struct {
	Content    string
	ChangeKind string
	Insertions int
	Deletions  int
}

func extractFileChanges(item map[string]json.RawMessage, workspaceRoot string) []fileChange {
	rawChanges, ok := rawFileChanges(item)
	if !ok {
		return nil
	}

	changes := make([]fileChange, 0, len(rawChanges))
	for _, change := range rawChanges {
		path := normalizeToolPath(rawString(change, "path"), workspaceRoot)
		if path == "" {
			continue
		}
		kind, movePath := normalizeChangeKind(change["kind"])
		movePath = normalizeToolPath(movePath, workspaceRoot)
		previousPath := ""
		if movePath != "" {
			previousPath = path
			path = movePath
			kind = "renamed"
		}
		changes = append(changes, fileChange{
			Path:         path,
			PreviousPath: previousPath,
			Kind:         kind,
			Diff:         rawString(change, "diff"),
		})
	}
	return changes
}

func rawFileChanges(item map[string]json.RawMessage) ([]map[string]json.RawMessage, bool) {
	if changes, ok := decodeFileChanges(item["changes"]); ok {
		return changes, true
	}

	dataRaw, ok := item["data"]
	if !ok {
		return nil, false
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(dataRaw, &data) != nil {
		return nil, false
	}
	itemDataRaw, ok := data["item"]
	if !ok {
		return nil, false
	}
	var itemData map[string]json.RawMessage
	if json.Unmarshal(itemDataRaw, &itemData) != nil {
		return nil, false
	}
	return decodeFileChanges(itemData["changes"])
}

func decodeFileChanges(raw json.RawMessage) ([]map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var changes []map[string]json.RawMessage
	if json.Unmarshal(raw, &changes) != nil || len(changes) == 0 {
		return nil, false
	}
	return changes, true
}

func buildInlineDiffFromChanges(changes []fileChange) (*ToolInlineDiff, string) {
	files := make([]ToolInlineDiffFile, 0, len(changes))
	var patchBuilder strings.Builder
	exact := true
	totalInsertions := 0
	totalDeletions := 0

	for _, change := range changes {
		file := ToolInlineDiffFile{
			Path:         change.Path,
			PreviousPath: change.PreviousPath,
			Kind:         change.Kind,
		}
		if patch, ok := buildUnifiedPatch(change); ok {
			if change.Kind == "renamed" {
				file.Kind = "renamed"
			} else {
				file.Kind = patch.ChangeKind
			}
			file.Insertions = patch.Insertions
			file.Deletions = patch.Deletions
			totalInsertions += patch.Insertions
			totalDeletions += patch.Deletions
			if patchBuilder.Len() > 0 {
				patchBuilder.WriteByte('\n')
			}
			patchBuilder.WriteString(patch.Content)
		} else {
			exact = false
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		return nil, ""
	}
	if !exact {
		return &ToolInlineDiff{
			Availability: "summary_only",
			Files:        files,
			Insertions:   totalInsertions,
			Deletions:    totalDeletions,
		}, ""
	}

	return &ToolInlineDiff{
			Availability: "exact_patch",
			Files:        files,
			Insertions:   totalInsertions,
			Deletions:    totalDeletions,
		},
		patchBuilder.String()
}

func buildUnifiedPatch(change fileChange) (fileChangePatch, bool) {
	hunk := strings.TrimSpace(change.Diff)

	var header []string
	switch change.Kind {
	case "added":
		var builder strings.Builder
		writePatchHeader(&builder,
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			"new file mode 100644",
			"--- /dev/null",
			fmt.Sprintf("+++ b/%s", change.Path),
		)
		insertions := writeContentDiffLines(&builder, change.Diff, "added")
		return fileChangePatch{
			Content:    builder.String(),
			ChangeKind: "added",
			Insertions: insertions,
		}, true
	case "deleted":
		var builder strings.Builder
		writePatchHeader(&builder,
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			"deleted file mode 100644",
			fmt.Sprintf("--- a/%s", change.Path),
			"+++ /dev/null",
		)
		deletions := writeContentDiffLines(&builder, change.Diff, "deleted")
		return fileChangePatch{
			Content:    builder.String(),
			ChangeKind: "deleted",
			Deletions:  deletions,
		}, true
	case "renamed":
		if change.PreviousPath == "" {
			return fileChangePatch{}, false
		}
		header = []string{
			fmt.Sprintf("diff --git a/%s b/%s", change.PreviousPath, change.Path),
			fmt.Sprintf("rename from %s", change.PreviousPath),
			fmt.Sprintf("rename to %s", change.Path),
			fmt.Sprintf("--- a/%s", change.PreviousPath),
			fmt.Sprintf("+++ b/%s", change.Path),
		}
		if hunk == "" {
			content := strings.Join(header, "\n")
			return fileChangePatch{
				Content:    content,
				ChangeKind: "renamed",
			}, true
		}
	default:
		if hunk == "" {
			return fileChangePatch{}, false
		}
		if strings.HasPrefix(hunk, "diff --git ") {
			meta := ExtractDiffMeta(hunk)
			return fileChangePatch{
				Content:    hunk,
				ChangeKind: meta.ChangeKind,
				Insertions: meta.Insertions,
				Deletions:  meta.Deletions,
			}, true
		}
		header = []string{
			fmt.Sprintf("diff --git a/%s b/%s", change.Path, change.Path),
			fmt.Sprintf("--- a/%s", change.Path),
			fmt.Sprintf("+++ b/%s", change.Path),
		}
	}

	content := strings.Join(append(header, hunk), "\n")
	meta := ExtractDiffMeta(content)
	return fileChangePatch{
		Content:    content,
		ChangeKind: meta.ChangeKind,
		Insertions: meta.Insertions,
		Deletions:  meta.Deletions,
	}, true
}

func writePatchHeader(builder *strings.Builder, lines ...string) {
	for i, line := range lines {
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
}

func writeContentDiffLines(builder *strings.Builder, content string, kind string) int {
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		builder.WriteString("\n@@ -0,0 +0,0 @@")
		return 0
	}

	lineCount := strings.Count(content, "\n") + 1
	prefix := "-"
	header := fmt.Sprintf("@@ -1,%d +0,0 @@", lineCount)
	if kind == "added" {
		prefix = "+"
		header = fmt.Sprintf("@@ -0,0 +1,%d @@", lineCount)
	}

	builder.WriteByte('\n')
	builder.WriteString(header)
	for {
		line := content
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			line = content[:index]
			content = content[index+1:]
			builder.WriteByte('\n')
			builder.WriteString(prefix)
			builder.WriteString(line)
			if content == "" {
				builder.WriteByte('\n')
				builder.WriteString(prefix)
				break
			}
			continue
		}
		builder.WriteByte('\n')
		builder.WriteString(prefix)
		builder.WriteString(line)
		break
	}
	return lineCount
}

func normalizeChangeKind(raw json.RawMessage) (string, string) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case "added", "create":
			return "added", ""
		case "deleted", "remove":
			return "deleted", ""
		case "renamed", "move":
			return "renamed", ""
		default:
			return "modified", ""
		}
	}

	var object struct {
		Type     string `json:"type"`
		MovePath string `json:"move_path"`
	}
	if json.Unmarshal(raw, &object) == nil {
		switch object.Type {
		case "add", "create":
			return "added", ""
		case "delete":
			return "deleted", ""
		case "move":
			return "renamed", object.MovePath
		default:
			return "modified", object.MovePath
		}
	}
	return "modified", ""
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
	if meta.ItemType == "file_change" && meta.Title != "" {
		return meta.Title
	}
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
	if meta.ItemType == "file_change" {
		return meta.Title
	}
	if meta.InlineDiff == nil || len(meta.InlineDiff.Files) == 0 {
		return meta.Title
	}
	if len(meta.InlineDiff.Files) == 1 {
		return meta.InlineDiff.Files[0].Path
	}
	return fmt.Sprintf("%d files changed", len(meta.InlineDiff.Files))
}

func fileChangeTitle(inlineDiff *ToolInlineDiff) string {
	if inlineDiff == nil || len(inlineDiff.Files) == 0 {
		return "File change"
	}
	if len(inlineDiff.Files) == 1 {
		file := inlineDiff.Files[0]
		verb := "Edited"
		switch file.Kind {
		case "added":
			verb = "Added"
		case "deleted":
			verb = "Deleted"
		}
		return fmt.Sprintf("%s %s %s", verb, displayInlineDiffPath(file), formatInlineDiffCounts(file.Insertions, file.Deletions))
	}
	return fmt.Sprintf("Edited %d files %s", len(inlineDiff.Files), formatInlineDiffCounts(inlineDiff.Insertions, inlineDiff.Deletions))
}

func displayInlineDiffPath(file ToolInlineDiffFile) string {
	if file.PreviousPath == "" {
		return file.Path
	}
	return file.PreviousPath + " -> " + file.Path
}

func formatInlineDiffCounts(insertions, deletions int) string {
	return fmt.Sprintf("(+%d -%d)", insertions, deletions)
}

func isFileChangeItemType(itemType string) bool {
	return isCodexFileChangeItem(itemType)
}

func toolResultPayloadID(itemID string) string {
	return "tool-result:" + itemID
}

func hasExactToolInlineDiff(inlineDiff *ToolInlineDiff) bool {
	return inlineDiff != nil && inlineDiff.Availability == "exact_patch"
}

func normalizeToolPath(rawPath, workspaceRoot string) string {
	return normalizeWorkspaceRelativePath(rawPath, workspaceRoot)
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
