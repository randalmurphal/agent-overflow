package triage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("tool result turn index: %w", err)
	}

	payloadID := toolResultPayloadID(evt.ItemID)
	meta, diffData = r.mergeToolResultPayload(payloadID, meta, diffData)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal tool result meta: %w", err)
	}

	payload := store.Payload{
		ID:        payloadID,
		Kind:      toolResultPayloadKind,
		Meta:      string(metaJSON),
		Data:      diffData,
		CreatedAt: now,
	}
	if err := r.store.UpsertPayload(payload); err != nil {
		return fmt.Errorf("persist tool result payload: %w", err)
	}
	r.emitPayloadMeta(payloadID, evt.ThreadID, toolResultPayloadKind, string(metaJSON), now)

	item, found, err := r.store.GetItem(evt.ItemID)
	if err != nil {
		return fmt.Errorf("lookup tool result item: %w", err)
	}
	summary := summarizeToolResult(meta)
	if found {
		return r.store.UpdateItemPayload(item.ID, payloadID, summary, now)
	}

	itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
	if err != nil {
		return fmt.Errorf("tool result item index: %w", err)
	}

	return r.store.InsertItem(store.Item{
		ID:        evt.ItemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      toolResultItemKind,
		Role:      "assistant",
		Summary:   summary,
		PayloadID: payloadID,
		CreatedAt: now,
	})
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

func (r *Router) upgradeSummaryOnlyToolResults(threadID string, turnIndex int, turnDiff string) error {
	if strings.TrimSpace(turnDiff) == "" {
		return nil
	}

	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("lookup thread for diff upgrade: %w", err)
	}
	items, err := r.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("list turn items for diff upgrade: %w", err)
	}

	candidates := make([]toolResultCandidate, 0, len(items))
	signatureCounts := map[string]int{}
	for _, item := range items {
		candidate, ok := r.loadSummaryOnlyToolResultCandidate(item)
		if !ok {
			continue
		}
		signature := inlineDiffPathSignature(candidate.meta.InlineDiff.Files, thread.WorkspacePath)
		if signature == "" {
			continue
		}
		candidate.pathSignature = signature
		candidates = append(candidates, candidate)
		signatureCounts[signature]++
	}

	for _, candidate := range candidates {
		if signatureCounts[candidate.pathSignature] != 1 {
			continue
		}

		filtered := filterUnifiedDiffByPaths(turnDiff, candidate.meta.InlineDiff.Files, thread.WorkspacePath)
		if strings.TrimSpace(filtered) == "" {
			continue
		}

		upgraded := buildExactInlineDiff(filtered)
		if upgraded == nil {
			continue
		}
		candidate.meta.InlineDiff = upgraded

		metaJSON, err := json.Marshal(candidate.meta)
		if err != nil {
			return fmt.Errorf("marshal upgraded tool result meta: %w", err)
		}

		now := time.Now().UnixMilli()
		if err := r.store.UpsertPayload(store.Payload{
			ID:        candidate.payloadID,
			Kind:      toolResultPayloadKind,
			Meta:      string(metaJSON),
			Data:      []byte(filtered),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("persist upgraded tool result payload: %w", err)
		}
		r.emitPayloadMeta(candidate.payloadID, threadID, toolResultPayloadKind, string(metaJSON), now)
		if err := r.store.UpdateItemPayload(candidate.item.ID, candidate.payloadID, summarizeToolResult(candidate.meta), now); err != nil {
			return fmt.Errorf("persist upgraded tool result item: %w", err)
		}
	}

	return nil
}

type toolResultCandidate struct {
	item          store.Item
	payloadID     string
	meta          ToolResultMeta
	pathSignature string
}

func (r *Router) loadSummaryOnlyToolResultCandidate(item store.Item) (toolResultCandidate, bool) {
	if item.Kind != toolResultItemKind || item.PayloadID == "" {
		return toolResultCandidate{}, false
	}
	pm, err := r.store.GetPayloadMeta(item.PayloadID)
	if err != nil || pm.Kind != toolResultPayloadKind {
		return toolResultCandidate{}, false
	}

	var meta ToolResultMeta
	if json.Unmarshal([]byte(pm.Meta), &meta) != nil {
		return toolResultCandidate{}, false
	}
	if meta.ItemType != "file_change" || meta.InlineDiff == nil || meta.InlineDiff.Availability != "summary_only" {
		return toolResultCandidate{}, false
	}
	if len(meta.InlineDiff.Files) == 0 {
		return toolResultCandidate{}, false
	}

	return toolResultCandidate{
		item:      item,
		payloadID: item.PayloadID,
		meta:      meta,
	}, true
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
		Title:    firstNonEmpty(rawString(item, "title"), "File change"),
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

func buildExactInlineDiff(diff string) *ToolInlineDiff {
	sections := splitUnifiedDiffSections(diff)
	if len(sections) == 0 {
		return nil
	}

	files := make([]ToolInlineDiffFile, 0, len(sections))
	insertions := 0
	deletions := 0
	for _, section := range sections {
		meta := ExtractDiffMeta(section)
		path := strings.TrimSpace(meta.FilePath)
		if path == "" {
			continue
		}
		files = append(files, ToolInlineDiffFile{
			Path:       path,
			Kind:       meta.ChangeKind,
			Insertions: meta.Insertions,
			Deletions:  meta.Deletions,
		})
		insertions += meta.Insertions
		deletions += meta.Deletions
	}
	if len(files) == 0 {
		return nil
	}
	return &ToolInlineDiff{
		Availability: "exact_patch",
		Files:        files,
		Insertions:   insertions,
		Deletions:    deletions,
	}
}

func splitUnifiedDiffSections(diff string) []string {
	lines := strings.Split(strings.TrimSpace(diff), "\n")
	var sections []string
	var current []string

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if len(current) > 0 {
				sections = append(sections, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) == 0 {
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		sections = append(sections, strings.Join(current, "\n"))
	}
	return sections
}

func filterUnifiedDiffByPaths(diff string, files []ToolInlineDiffFile, workspaceRoot string) string {
	if len(files) == 0 {
		return ""
	}

	allowed := make(map[string]struct{}, len(files))
	for _, file := range files {
		path := normalizeToolPath(file.Path, workspaceRoot)
		if path != "" {
			allowed[path] = struct{}{}
		}
	}

	var sections []string
	for _, section := range splitUnifiedDiffSections(diff) {
		if toolDiffSectionAllowed(section, allowed, workspaceRoot) {
			sections = append(sections, section)
		}
	}
	return strings.Join(sections, "\n")
}

func toolDiffSectionAllowed(section string, allowed map[string]struct{}, workspaceRoot string) bool {
	for _, path := range diffSectionPaths(section, workspaceRoot) {
		if _, ok := allowed[path]; ok {
			return true
		}
	}
	return false
}

func diffSectionPaths(section string, workspaceRoot string) []string {
	var paths []string
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			paths = append(paths, normalizeToolPath(strings.TrimPrefix(line, "+++ b/"), workspaceRoot))
		case strings.HasPrefix(line, "rename to "):
			paths = append(paths, normalizeToolPath(strings.TrimPrefix(line, "rename to "), workspaceRoot))
		case strings.HasPrefix(line, "diff --git "):
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				paths = append(paths, normalizeToolPath(strings.TrimPrefix(fields[3], "b/"), workspaceRoot))
			}
		}
	}
	return uniqueNonEmpty(paths)
}

func inlineDiffPathSignature(files []ToolInlineDiffFile, workspaceRoot string) string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path := normalizeToolPath(file.Path, workspaceRoot)
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	return strings.Join(paths, "|")
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
