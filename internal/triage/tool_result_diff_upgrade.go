package triage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

func (r *Router) upgradeSummaryOnlyToolResults(threadID string, turnIndex int, turnDiff string) (bool, error) {
	if strings.TrimSpace(turnDiff) == "" {
		return false, nil
	}

	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return false, fmt.Errorf("lookup thread for diff upgrade: %w", err)
	}
	items, err := r.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return false, fmt.Errorf("list turn items for diff upgrade: %w", err)
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

	upgradedAny := false
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
			return false, fmt.Errorf("marshal upgraded tool result meta: %w", err)
		}

		now := time.Now().UnixMilli()
		payload := store.Payload{
			ID:        candidate.payloadID,
			Kind:      toolResultPayloadKind,
			Meta:      string(metaJSON),
			Data:      []byte(filtered),
			CreatedAt: now,
		}
		candidate.item.PayloadID = candidate.payloadID
		candidate.item.Summary = summarizeToolResult(candidate.meta)
		candidate.item.UpdatedAt = now
		if err := r.persistItem(candidate.item, &payload); err != nil {
			return false, fmt.Errorf("persist upgraded tool result item: %w", err)
		}
		r.notifyDiffPayloadPersisted(threadID, candidate.payloadID, candidate.meta, filtered)
		upgradedAny = true
	}

	return upgradedAny, nil
}

type toolResultCandidate struct {
	item          store.Item
	payloadID     string
	meta          ToolResultMeta
	pathSignature string
}

func (r *Router) loadSummaryOnlyToolResultCandidate(item store.Item) (toolResultCandidate, bool) {
	// Accept both legacy "tool_result" rows and the new lifecycle
	// "tool_call" rows that carry a tool_result payload — after the
	// v14 reshape the tool_call lifecycle item owns the rich payload
	// for file-change tools, but the upgrade target is the same.
	if item.Kind != toolResultItemKind && item.Kind != itemKindToolCall {
		return toolResultCandidate{}, false
	}
	if item.PayloadID == "" {
		return toolResultCandidate{}, false
	}
	// ListTurnItems left-joins payloads so PayloadKind and PayloadMeta
	// arrive hydrated on the row already — the former GetPayloadMeta
	// round-trip was pure N×1 overhead per turn on diff-upgrade.
	if item.PayloadKind != toolResultPayloadKind {
		return toolResultCandidate{}, false
	}

	var meta ToolResultMeta
	if json.Unmarshal([]byte(item.PayloadMeta), &meta) != nil {
		return toolResultCandidate{}, false
	}
	if meta.ItemType != "file_change" || meta.InlineDiff == nil || meta.InlineDiff.Availability != "summary_only" {
		return toolResultCandidate{}, false
	}
	if len(meta.InlineDiff.Files) == 0 {
		return toolResultCandidate{}, false
	}
	if meta.InlineDiff.FilesTruncated || meta.InlineDiff.OmittedFiles > 0 {
		return toolResultCandidate{}, false
	}

	return toolResultCandidate{
		item:      item,
		payloadID: item.PayloadID,
		meta:      meta,
	}, true
}

func buildExactInlineDiff(diff string) *ToolInlineDiff {
	sections := splitUnifiedDiffSections(diff)
	if len(sections) == 0 {
		return nil
	}

	files := make([]ToolInlineDiffFile, 0, min(len(sections), inlineDiffPreviewFileCount))
	insertions := 0
	deletions := 0
	totalFiles := 0
	for _, section := range sections {
		meta := ExtractDiffMeta(section)
		path := strings.TrimSpace(meta.FilePath)
		if path == "" {
			continue
		}
		totalFiles++
		file := ToolInlineDiffFile{
			Path:         path,
			PreviousPath: diffSectionPreviousPath(section),
			Kind:         meta.ChangeKind,
			Insertions:   meta.Insertions,
			Deletions:    meta.Deletions,
		}
		applyInlineDiffPreview(&file, section)
		if len(files) < inlineDiffPreviewFileCount {
			files = append(files, file)
		}
		insertions += meta.Insertions
		deletions += meta.Deletions
	}
	if totalFiles == 0 {
		return nil
	}
	return &ToolInlineDiff{
		Availability:   "exact_patch",
		Files:          files,
		TotalFiles:     totalFiles,
		OmittedFiles:   omittedInlineDiffFiles(totalFiles, len(files)),
		FilesTruncated: totalFiles > len(files),
		Insertions:     insertions,
		Deletions:      deletions,
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

func diffSectionPreviousPath(section string) string {
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "rename from ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "rename from "))
		}
	}
	return ""
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
