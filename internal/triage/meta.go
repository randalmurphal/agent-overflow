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
	Command      string `json:"command"`
	ExitCode     int    `json:"exitCode"`
	LineCount    int    `json:"lineCount"`
	Preview      string `json:"preview,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	OutputState  string `json:"outputFileState,omitempty"`
}

// ThinkingMeta is the JSON structure for thinking block payloads.
type ThinkingMeta struct {
	TokenCount int    `json:"tokenCount"`
	Preview    string `json:"preview"`
	Signature  string `json:"signature,omitempty"`
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

// ExtractCommandOutputMeta extracts lightweight command output metadata.
// The output body itself stays behind the payload API and loads only on
// explicit expand, so command output metadata intentionally carries no
// preview text.
func ExtractCommandOutputMeta(output string, command string, exitCode int) CommandOutputMeta {
	cm := CommandOutputMeta{
		Command:   command,
		ExitCode:  exitCode,
		LineCount: strings.Count(output, "\n") + 1,
	}
	return cm
}

// ExtractCommandOutputMetaWithError adds a compact one-line failure
// message for collapsed command rows. The full output stays in the
// payload and is fetched only when the row expands.
func ExtractCommandOutputMetaWithError(output string, command string, exitCode int, errorMessage string) CommandOutputMeta {
	cm := ExtractCommandOutputMeta(output, command, exitCode)
	cm.ErrorMessage = compactCommandErrorMessage(errorMessage)
	if cm.ErrorMessage == "" && exitCode != 0 {
		cm.ErrorMessage = compactCommandErrorMessage(output)
	}
	return cm
}

func compactCommandErrorMessage(text string) string {
	tailLines := compactTailLines(text, 2)
	if len(tailLines) == 0 {
		return ""
	}
	message := strings.Join(tailLines, " ")
	runes := []rune(message)
	if len(runes) > 240 {
		return strings.TrimSpace(string(runes[:239])) + "…"
	}
	return message
}

func compactTailLines(text string, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	lines := make([]string, 0, maxLines)
	end := len(text)
	for end >= 0 && len(lines) < maxLines {
		start := strings.LastIndexByte(text[:end], '\n')
		line := text[start+1 : end]
		compact := compactCommandLine(line)
		if compact != "" {
			lines = append(lines, compact)
		}
		if start < 0 {
			break
		}
		end = start
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return lines
}

func compactCommandLine(text string) string {
	return strings.Join(strings.Fields(stripANSIControlSequences(text)), " ")
}

func stripANSIControlSequences(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b || i+1 >= len(text) || text[i+1] != '[' {
			b.WriteByte(text[i])
			continue
		}
		i += 2
		for i < len(text) {
			c := text[i]
			if c >= 0x40 && c <= 0x7e {
				break
			}
			i++
		}
	}
	return b.String()
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
