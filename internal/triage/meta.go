package triage

import (
	"strings"
	"unicode/utf8"
)

// DiffMeta is the JSON structure stored in payloads.meta for diffs.
type DiffMeta struct {
	FilePath   string `json:"filePath"`
	ChangeKind string `json:"changeKind"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Preview    string `json:"preview"`
}

// CommandOutputMeta is the JSON structure for command output payloads.
type CommandOutputMeta struct {
	Command   string `json:"command"`
	ExitCode  int    `json:"exitCode"`
	LineCount int    `json:"lineCount"`
	Preview   string `json:"preview"`
}

// ThinkingMeta is the JSON structure for thinking block payloads.
type ThinkingMeta struct {
	TokenCount int    `json:"tokenCount"`
	Preview    string `json:"preview"`
}

// ProposedPlanMeta is the JSON structure for proposed plan payloads.
type ProposedPlanMeta struct {
	Title     string `json:"title"`
	LineCount int    `json:"lineCount"`
	CharCount int    `json:"charCount"`
	Preview   string `json:"preview"`
}

// ExtractDiffMeta parses a unified diff string and returns structured meta.
func ExtractDiffMeta(patch string) DiffMeta {
	dm := DiffMeta{ChangeKind: "modified"}
	lines := strings.Split(patch, "\n")

	// Extract file path from diff header.
	for _, line := range lines {
		if strings.HasPrefix(line, "+++ b/") {
			dm.FilePath = strings.TrimPrefix(line, "+++ b/")
			break
		}
		if strings.HasPrefix(line, "+++ ") {
			dm.FilePath = strings.TrimPrefix(line, "+++ ")
			break
		}
	}

	// Count insertions/deletions (lines starting with + or - that aren't headers).
	inBody := false
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			inBody = true
			continue
		}
		if !inBody {
			continue
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			dm.Insertions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			dm.Deletions++
		}
	}

	// Detect change kind.
	if dm.Deletions == 0 && dm.Insertions > 0 {
		for _, line := range lines {
			if strings.HasPrefix(line, "new file") {
				dm.ChangeKind = "added"
				break
			}
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "deleted file") {
			dm.ChangeKind = "deleted"
			break
		}
		if strings.HasPrefix(line, "rename from") {
			dm.ChangeKind = "renamed"
			break
		}
	}

	// Preview: first ~20 lines.
	previewLines := lines
	if len(previewLines) > 20 {
		previewLines = previewLines[:20]
	}
	dm.Preview = strings.Join(previewLines, "\n")

	return dm
}

// ExtractCommandOutputMeta extracts preview from command output.
func ExtractCommandOutputMeta(output string, command string, exitCode int) CommandOutputMeta {
	lines := strings.Split(output, "\n")
	cm := CommandOutputMeta{
		Command:   command,
		ExitCode:  exitCode,
		LineCount: len(lines),
	}

	// Preview: last ~10 lines.
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	cm.Preview = strings.Join(lines[start:], "\n")

	return cm
}

// ExtractThinkingMeta extracts preview from a thinking block.
func ExtractThinkingMeta(content string) ThinkingMeta {
	tm := ThinkingMeta{
		// Rough token estimate: runes / 4.
		TokenCount: utf8.RuneCountInString(content) / 4,
	}

	// Preview: first ~200 runes (truncating at byte boundaries can split
	// multi-byte UTF-8 characters and produce invalid output).
	runes := []rune(content)
	if len(runes) > 200 {
		tm.Preview = string(runes[:200]) + "..."
	} else {
		tm.Preview = content
	}

	return tm
}

// ExtractProposedPlanMeta builds lightweight metadata for proposed plan cards.
func ExtractProposedPlanMeta(planMarkdown string) ProposedPlanMeta {
	trimmed := strings.TrimSpace(planMarkdown)
	lines := strings.Split(strings.TrimRight(trimmed, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}

	return ProposedPlanMeta{
		Title:     proposedPlanTitle(trimmed),
		LineCount: len(lines),
		CharCount: len(trimmed),
		Preview:   buildCollapsedPlanPreview(trimmed, 10),
	}
}

func proposedPlanTitle(planMarkdown string) string {
	for _, line := range strings.Split(planMarkdown, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			title := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if title != "" {
				return title
			}
		}
	}
	return "Proposed plan"
}

func buildCollapsedPlanPreview(planMarkdown string, maxLines int) string {
	sourceLines := strings.Split(strings.TrimRight(stripDisplayedPlanMarkdown(planMarkdown), "\n"), "\n")
	var preview []string
	visibleLines := 0
	hasMore := false

	for _, line := range sourceLines {
		isVisible := strings.TrimSpace(line) != ""
		if isVisible && visibleLines >= maxLines {
			hasMore = true
			break
		}
		preview = append(preview, strings.TrimRight(line, " "))
		if isVisible {
			visibleLines++
		}
	}

	for len(preview) > 0 && strings.TrimSpace(preview[len(preview)-1]) == "" {
		preview = preview[:len(preview)-1]
	}
	if len(preview) == 0 {
		return proposedPlanTitle(planMarkdown)
	}
	if hasMore {
		preview = append(preview, "", "...")
	}
	return strings.Join(preview, "\n")
}

func stripDisplayedPlanMarkdown(planMarkdown string) string {
	lines := strings.Split(strings.TrimRight(planMarkdown, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "#") {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.EqualFold(strings.TrimSpace(strings.TrimLeft(lines[0], "# ")), "summary") {
		lines = lines[1:]
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}
