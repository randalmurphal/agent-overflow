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
