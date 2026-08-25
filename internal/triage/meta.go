package triage

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
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
	// DevServerURL is the loopback URL this command announced, if any
	// (see dev_server_url.go). Present so a collapsed command row can
	// offer "open in browser" without loading the output blob. While the
	// row streams this reflects the current flush window only — the
	// completion rebuild recomputes it over the cumulative payload, and
	// the row keeps the first detection for its visible lifetime.
	DevServerURL string `json:"devServerUrl,omitempty"`
}

// ThinkingMeta is the JSON structure for thinking block payloads.
type ThinkingMeta struct {
	TokenCount int    `json:"tokenCount"`
	Preview    string `json:"preview"`
	Signature  string `json:"signature,omitempty"`
}

// CompactionMeta is the cheap, always-loaded view of a compaction payload.
// Claudetui commits the summarizer's summary onto the live boundary; Claude
// subagent transcripts also pair their exact boundary and compact-summary
// rows during import or terminal reconciliation. This meta lets the frontend
// label the expandable divider without pulling the full summary. Summarizer
// reasoning streams separately as `compaction_reasoning` and is not part of
// this view. Codex and headless Claude's top-level boundary carry no summary.
type CompactionMeta struct {
	SummaryPreview string `json:"summaryPreview,omitempty"`
	SummaryChars   int    `json:"summaryChars"`
}

// ProposedPlanMeta is the JSON structure for proposed plan payloads.
type ProposedPlanMeta struct {
	Title            string `json:"title"`
	LineCount        int    `json:"lineCount"`
	CharCount        int    `json:"charCount"`
	Preview          string `json:"preview"`
	Signature        string `json:"signature,omitempty"`
	PreviewTruncated bool   `json:"previewTruncated"`
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
		Command:      command,
		ExitCode:     exitCode,
		LineCount:    strings.Count(output, "\n") + 1,
		DevServerURL: DetectDevServerURL(output),
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

// stripANSIControlSequences removes ANSI/OSC escape sequences from a command
// line, preserving every other byte (case, spacing) for the compact preview.
// The escape-boundary logic is shared with the claudetui PTY scan via
// stringsx.SkipANSIEscape, so this also drops OSC / charset / bare-ESC sequences
// that the old CSI-only stripper used to leak into the preview.
func stripANSIControlSequences(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == 0x1b {
			i = stringsx.SkipANSIEscape(text, i)
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// metaPreviewRunes caps the rune length of the cheap text preview stored in
// item meta (thinking deltas and the committed compaction summary) so the
// frontend can label a row without loading the payload blob.
const metaPreviewRunes = 200

// ExtractThinkingMeta extracts preview from a thinking block.
func ExtractThinkingMeta(content string) ThinkingMeta {
	return ThinkingMeta{
		// Rough token estimate: runes / 4.
		TokenCount: utf8.RuneCountInString(content) / 4,
		Preview:    truncateRunes(content, metaPreviewRunes),
	}
}

// ExtractCompactionMeta builds the cheap meta view for a compaction payload
// from the committed summary — the user-facing headline of a compaction. The
// preview + size let the frontend label the expandable row without loading the
// data blob.
func ExtractCompactionMeta(summary string) CompactionMeta {
	return CompactionMeta{
		SummaryPreview: truncateRunes(summary, metaPreviewRunes),
		SummaryChars:   utf8.RuneCountInString(summary),
	}
}

// ExtractProposedPlanMeta builds lightweight metadata for proposed plan cards.
func ExtractProposedPlanMeta(planMarkdown string) ProposedPlanMeta {
	trimmed := strings.TrimSpace(planMarkdown)
	lines := strings.Split(strings.TrimRight(trimmed, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	preview, previewTruncated := buildCollapsedPlanPreview(trimmed, 10)
	signature := sha256.Sum256([]byte(trimmed))

	return ProposedPlanMeta{
		Title:            proposedPlanTitle(trimmed),
		LineCount:        len(lines),
		CharCount:        len(trimmed),
		Preview:          preview,
		Signature:        fmt.Sprintf("sha256:%x", signature),
		PreviewTruncated: previewTruncated,
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

func buildCollapsedPlanPreview(planMarkdown string, maxLines int) (string, bool) {
	sourceLines := strings.Split(strings.TrimRight(planMarkdown, "\n"), "\n")
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
		return proposedPlanTitle(planMarkdown), false
	}
	if hasMore {
		preview = append(preview, "", "...")
	}
	return strings.Join(preview, "\n"), hasMore
}
