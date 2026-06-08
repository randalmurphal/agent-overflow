package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

const (
	toolResultPayloadKind      = "tool_result"
	toolResultItemKind         = "tool_result"
	inlineDiffPreviewLineCount = 30
	inlineDiffPreviewFileCount = 25
)

type ToolResultMeta struct {
	ItemType   string          `json:"itemType"`
	Title      string          `json:"title"`
	Detail     string          `json:"detail,omitempty"`
	Preview    string          `json:"preview,omitempty"`
	InlineDiff *ToolInlineDiff `json:"inlineDiff,omitempty"`
}

type ToolInlineDiff struct {
	Availability   string               `json:"availability"`
	Files          []ToolInlineDiffFile `json:"files"`
	TotalFiles     int                  `json:"totalFiles,omitempty"`
	OmittedFiles   int                  `json:"omittedFiles,omitempty"`
	FilesTruncated bool                 `json:"filesTruncated,omitempty"`
	Insertions     int                  `json:"insertions,omitempty"`
	Deletions      int                  `json:"deletions,omitempty"`
}

type ToolInlineDiffFile struct {
	Path             string `json:"path"`
	PreviousPath     string `json:"previousPath,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Insertions       int    `json:"insertions,omitempty"`
	Deletions        int    `json:"deletions,omitempty"`
	PreviewPatch     string `json:"previewPatch,omitempty"`
	PreviewLineCount int    `json:"previewLineCount,omitempty"`
	PreviewTruncated bool   `json:"previewTruncated,omitempty"`
}

func (r *Router) persistFileChangeToolResult(evt provider.ProviderEvent) error {
	if evt.ItemID == "" || len(evt.Meta) == 0 {
		return nil
	}

	// Resolve the tool name. Codex and the Claude EventToolStart path
	// stamp ItemType directly. Claude's EventToolComplete leaves
	// ItemType empty (parse_user.go's appendToolResultCompletion never
	// sets it), so we recover the tool name from the persisted
	// tool_call row's ToolName. Same lookup also surfaces the file
	// path the tool committed to write at start, used as a fallback
	// for the new Claude extractor when tool_use_result.filePath is
	// missing on older wire shapes.
	toolName := evt.ItemType
	claudeFallbackFilePath := ""
	if toolName == "" && evt.Kind == provider.EventToolComplete {
		if existing, found, err := r.store.GetThreadItem(evt.ThreadID, evt.ItemID); err == nil && found {
			toolName = existing.ToolName
			claudeFallbackFilePath = extractClaudeLaunchFilePath(existing.Meta)
		}
	}

	if !isFileChangeItemType(toolName) {
		return nil
	}

	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("lookup thread for tool result: %w", err)
	}

	var (
		meta     ToolResultMeta
		diffData []byte
		ok       bool
	)
	if isClaudeFilePathTool(toolName) {
		meta, diffData, ok = extractClaudeFileChangeToolResult(evt.Meta, toolName, claudeFallbackFilePath, thread.WorkspacePath)
	} else {
		meta, diffData, ok = extractFileChangeToolResult(evt.Meta, thread.WorkspacePath)
	}
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
	files := make([]ToolInlineDiffFile, 0, min(len(changes), inlineDiffPreviewFileCount))
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
			applyInlineDiffPreview(&file, patch.Content)
			totalInsertions += patch.Insertions
			totalDeletions += patch.Deletions
			if patchBuilder.Len() > 0 {
				patchBuilder.WriteByte('\n')
			}
			patchBuilder.WriteString(patch.Content)
		} else {
			exact = false
		}
		if len(files) < inlineDiffPreviewFileCount {
			files = append(files, file)
		}
	}

	totalFiles := len(changes)
	if totalFiles == 0 {
		return nil, ""
	}
	if !exact {
		return &ToolInlineDiff{
			Availability:   "summary_only",
			Files:          files,
			TotalFiles:     totalFiles,
			OmittedFiles:   omittedInlineDiffFiles(totalFiles, len(files)),
			FilesTruncated: totalFiles > len(files),
			Insertions:     totalInsertions,
			Deletions:      totalDeletions,
		}, ""
	}

	return &ToolInlineDiff{
			Availability:   "exact_patch",
			Files:          files,
			TotalFiles:     totalFiles,
			OmittedFiles:   omittedInlineDiffFiles(totalFiles, len(files)),
			FilesTruncated: totalFiles > len(files),
			Insertions:     totalInsertions,
			Deletions:      totalDeletions,
		},
		patchBuilder.String()
}

func omittedInlineDiffFiles(totalFiles, renderedFiles int) int {
	if totalFiles <= renderedFiles {
		return 0
	}
	return totalFiles - renderedFiles
}

func applyInlineDiffPreview(file *ToolInlineDiffFile, patch string) {
	preview, lineCount, truncated := lineBoundedDiffPreview(patch, inlineDiffPreviewLineCount)
	file.PreviewPatch = preview
	file.PreviewLineCount = lineCount
	file.PreviewTruncated = truncated
}

func lineBoundedDiffPreview(patch string, maxBodyLines int) (string, int, bool) {
	if strings.TrimSpace(patch) == "" {
		return "", 0, false
	}

	var preview strings.Builder
	writePreviewLine := func(line string) {
		if preview.Len() > 0 {
			preview.WriteByte('\n')
		}
		preview.WriteString(line)
	}

	remaining := strings.TrimSuffix(patch, "\n")
	bodyLines := 0
	truncated := false
	inHunk := false

	for remaining != "" {
		line, rest, found := strings.Cut(remaining, "\n")
		if !found {
			rest = ""
		}

		if isInlineDiffPreviewMetaLine(line, inHunk) {
			writePreviewLine(line)
			inHunk = nextInlineDiffPreviewHunkState(line, inHunk)
			remaining = rest
			continue
		}

		if bodyLines >= maxBodyLines {
			truncated = true
			break
		}
		writePreviewLine(line)
		bodyLines++

		if bodyLines >= maxBodyLines && hasInlineDiffPreviewBodyLine(rest, inHunk) {
			truncated = true
			break
		}
		remaining = rest
	}

	return preview.String(), bodyLines, truncated
}

func hasInlineDiffPreviewBodyLine(patch string, inHunk bool) bool {
	remaining := strings.TrimSuffix(patch, "\n")
	for remaining != "" {
		line, rest, found := strings.Cut(remaining, "\n")
		if !found {
			rest = ""
		}
		if !isInlineDiffPreviewMetaLine(line, inHunk) {
			return true
		}
		inHunk = nextInlineDiffPreviewHunkState(line, inHunk)
		remaining = rest
	}
	return false
}

func isInlineDiffPreviewMetaLine(line string, inHunk bool) bool {
	if strings.HasPrefix(line, "@@") {
		return true
	}
	if inHunk {
		return false
	}
	return strings.HasPrefix(line, "diff ") ||
		strings.HasPrefix(line, "---") ||
		strings.HasPrefix(line, "+++") ||
		strings.HasPrefix(line, "index ") ||
		strings.HasPrefix(line, "new file mode ") ||
		strings.HasPrefix(line, "deleted file mode ") ||
		strings.HasPrefix(line, "old mode ") ||
		strings.HasPrefix(line, "new mode ") ||
		strings.HasPrefix(line, "similarity index ") ||
		strings.HasPrefix(line, "dissimilarity index ") ||
		strings.HasPrefix(line, "rename from ") ||
		strings.HasPrefix(line, "rename to ") ||
		strings.HasPrefix(line, "copy from ") ||
		strings.HasPrefix(line, "copy to ")
}

func nextInlineDiffPreviewHunkState(line string, current bool) bool {
	if strings.HasPrefix(line, "diff ") {
		return false
	}
	if strings.HasPrefix(line, "@@") {
		return true
	}
	return current
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
			return buildValidatedFullUnifiedPatch(change, hunk)
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

func buildValidatedFullUnifiedPatch(change fileChange, patch string) (fileChangePatch, bool) {
	sections := splitUnifiedDiffSections(patch)
	if len(sections) != 1 {
		return fileChangePatch{}, false
	}
	section := sections[0]
	if !diffSectionMatchesFileChange(section, change) {
		return fileChangePatch{}, false
	}
	meta := ExtractDiffMeta(section)
	return fileChangePatch{
		Content:    section,
		ChangeKind: meta.ChangeKind,
		Insertions: meta.Insertions,
		Deletions:  meta.Deletions,
	}, true
}

func diffSectionMatchesFileChange(section string, change fileChange) bool {
	allowed := map[string]struct{}{change.Path: {}}
	if change.PreviousPath != "" {
		allowed[change.PreviousPath] = struct{}{}
	}
	paths, ok := strictDiffHeaderPaths(section)
	if !ok || len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if _, ok := allowed[path]; !ok {
			return false
		}
	}
	return true
}

func strictDiffHeaderPaths(section string) ([]string, bool) {
	paths := []string{}
	for _, line := range strings.Split(section, "\n") {
		var ok bool
		switch {
		case strings.HasPrefix(line, "diff --git "):
			fields := strings.Fields(line)
			if len(fields) < 4 {
				return nil, false
			}
			paths, ok = appendStrictDiffPath(paths, fields[2])
			if !ok {
				return nil, false
			}
			paths, ok = appendStrictDiffPath(paths, fields[3])
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, "--- "):
			paths, ok = appendStrictDiffPath(paths, strings.TrimPrefix(line, "--- "))
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, "+++ "):
			paths, ok = appendStrictDiffPath(paths, strings.TrimPrefix(line, "+++ "))
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, "rename from "):
			paths, ok = appendStrictDiffPath(paths, strings.TrimPrefix(line, "rename from "))
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, "rename to "):
			paths, ok = appendStrictDiffPath(paths, strings.TrimPrefix(line, "rename to "))
			if !ok {
				return nil, false
			}
		}
	}
	return uniqueNonEmpty(paths), true
}

func appendStrictDiffPath(paths []string, raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if fields := strings.Fields(raw); len(fields) > 0 {
		raw = fields[0]
	}
	if raw == "/dev/null" {
		return paths, true
	}
	raw = strings.TrimPrefix(raw, "a/")
	raw = strings.TrimPrefix(raw, "b/")
	path := normalizeToolPath(raw, "")
	if path == "" {
		return nil, false
	}
	return append(paths, path), true
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
	if inlineDiffTotalFiles(meta.InlineDiff) == 1 {
		return meta.InlineDiff.Files[0].Path
	}
	return fmt.Sprintf("%d files changed", inlineDiffTotalFiles(meta.InlineDiff))
}

func fileChangeTitle(inlineDiff *ToolInlineDiff) string {
	if inlineDiff == nil || len(inlineDiff.Files) == 0 {
		return "File change"
	}
	totalFiles := inlineDiffTotalFiles(inlineDiff)
	if totalFiles == 1 {
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
	return fmt.Sprintf("Edited %d files %s", totalFiles, formatInlineDiffCounts(inlineDiff.Insertions, inlineDiff.Deletions))
}

func inlineDiffTotalFiles(inlineDiff *ToolInlineDiff) int {
	if inlineDiff == nil {
		return 0
	}
	if inlineDiff.TotalFiles > 0 {
		return inlineDiff.TotalFiles
	}
	return len(inlineDiff.Files)
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
