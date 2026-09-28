package highlight

import (
	"strings"

	"agent-overflow/internal/unidiff"
)

// Unified-diff parsing for span alignment. The response contract is
// patch-aligned: result line i corresponds to patch text line i (as
// the frontend splits it), so the frontend indexes spans by a
// PatchLine's position with zero bookkeeping. Meta lines (@@ headers,
// file headers, `\ No newline`) get plain spans.
//
// Line classification is unidiff.Body's, which the frontend's
// patchFiles.ts shares:
//   - a hunk's body is the lines its header counts; in it `+` → add,
//     `-` → del, ` ` or empty → context, even when the text after the
//     prefix makes the line read `+++` or `---`
//   - add/del spans cover the prefix-STRIPPED body (content[1:])
//   - context spans cover the FULL content including its leading
//     space (the frontend's stripPatchLinePrefix passes context lines
//     through unchanged), so context runs get a 1-byte plain pad
//
// Each hunk reconstructs its own old/new virtual documents — hunks
// are parsed independently rather than concatenated, so a construct
// left open at the end of one hunk cannot poison the next hunk's
// grammar state across the invisible gap between them.

type hunkSide byte

const (
	sideNone hunkSide = iota
	sideOld
	sideNew
	sideBoth
)

type hunkLineRef struct {
	patchIndex  int // index into the patch's \n-split line sequence
	side        hunkSide
	oldDocLine  int // 0-based line within the hunk's old doc (-1 n/a)
	newDocLine  int // 0-based line within the hunk's new doc (-1 n/a)
	newFileLine int // 1-based new-side file line (-1 for del lines)
	outPad      int // plain bytes to prepend to output runs (context's leading space)
}

type patchHunk struct {
	oldStart, newStart int // 1-based file line numbers from the @@ header
	newCount           int // new-side line count (context + adds)
	oldDoc, newDoc     []byte
	lines              []hunkLineRef
}

type parsedPatch struct {
	lineCount int
	hunks     []patchHunk
}

// parsePatch parses one file's unified diff (the frontend sends one
// PatchFile's joined lines). Input outside hunk bodies is meta by
// construction; a malformed hunk header or a line its body cannot hold
// ends the current hunk rather than erroring — degraded output is plain
// spans, never a failure.
func parsePatch(patch string) parsedPatch {
	lines := strings.Split(patch, "\n")
	// A newline-terminated patch splits into a trailing empty segment
	// that is not a diff line; the frontend skips it the same way
	// (patchFiles.ts parsePatchFiles), so span indices stay aligned.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	result := parsedPatch{lineCount: len(lines)}

	var current *patchHunk
	var oldBody, newBody strings.Builder
	oldLines, newLines := 0, 0
	newFileLine := 0

	finish := func() {
		if current == nil {
			return
		}
		current.oldDoc = []byte(strings.TrimSuffix(oldBody.String(), "\n"))
		current.newDoc = []byte(strings.TrimSuffix(newBody.String(), "\n"))
		current.newCount = newLines
		result.hunks = append(result.hunks, *current)
		current = nil
		oldBody.Reset()
		newBody.Reset()
		oldLines, newLines = 0, 0
	}

	var body unidiff.Body
	for i, line := range lines {
		switch body.Next(line) {
		case unidiff.HunkHeader:
			finish()
			if hunk, ok := unidiff.ParseHunkHeader(line); ok {
				current = &patchHunk{oldStart: hunk.OldStart, newStart: hunk.NewStart}
				newFileLine = hunk.NewStart
			}
		case unidiff.Outside:
			// File headers, anything before the first hunk, or past a body.
			finish()
		case unidiff.NoNewline:
			// "\ No newline at end of file" — marker, not content.
		case unidiff.Added:
			newBody.WriteString(line[1:])
			newBody.WriteByte('\n')
			current.lines = append(current.lines, hunkLineRef{
				patchIndex: i, side: sideNew,
				oldDocLine: -1, newDocLine: newLines,
				newFileLine: newFileLine,
			})
			newLines++
			newFileLine++
		case unidiff.Removed:
			oldBody.WriteString(line[1:])
			oldBody.WriteByte('\n')
			current.lines = append(current.lines, hunkLineRef{
				patchIndex: i, side: sideOld,
				oldDocLine: oldLines, newDocLine: -1,
				newFileLine: -1,
			})
			oldLines++
		case unidiff.Context:
			// The body drops the leading space when present;
			// the output pad restores alignment with the frontend's
			// unstripped context content.
			text := line
			pad := 0
			if strings.HasPrefix(line, " ") {
				text = line[1:]
				pad = 1
			}
			oldBody.WriteString(text)
			oldBody.WriteByte('\n')
			newBody.WriteString(text)
			newBody.WriteByte('\n')
			current.lines = append(current.lines, hunkLineRef{
				patchIndex: i, side: sideBoth,
				oldDocLine: oldLines, newDocLine: newLines,
				newFileLine: newFileLine, outPad: pad,
			})
			oldLines++
			newLines++
			newFileLine++
		}
	}
	finish()
	return result
}

// padRuns prepends `pad` plain bytes to a line's runs (context lines
// keep their leading space in the frontend's stripped text).
func padRuns(line EncodedLine, pad int) EncodedLine {
	if pad == 0 || line.Runs == nil {
		return line
	}
	runs := make([]uint16, 0, len(line.Runs)+2)
	runs = append(runs, uint16(pad), ClassNone)
	runs = append(runs, line.Runs...)
	return EncodedLine{Runs: runs}
}

// ExpandLeadingTabs replaces a line's leading tab run with two spaces
// per tab — byte-for-byte the transform Claude Code applies to file
// content before computing an edit's structuredPatch (cli 2.1.212:
// `e.replace(/^\t+/gm, (t) => "  ".repeat(t.length))`). Interior tabs
// are preserved, matching the CLI. Patch verification tolerates this
// one divergence (a tab-indented file can never byte-match its own
// Claude edit diff otherwise), and edits-scope context expansion
// applies it to served lines so they indent like the hunk lines they
// sit between.
func ExpandLeadingTabs(line string) string {
	n := 0
	for n < len(line) && line[n] == '\t' {
		n++
	}
	if n == 0 {
		return line
	}
	return strings.Repeat("  ", n) + line[n:]
}

// PatchMatchesContent reports whether every new-side line of the
// patch's hunks (adds and context) matches `content` at its 1-based
// new-side line number. True means the file still holds the state
// this patch produced at every position the patch describes — the
// gate for treating live file content as a stand-in for a historical
// edit diff (parse priming, hunk-gap expansion). False on any
// mismatch, and on patches with no verifiable new-side lines (pure
// deletions, no hunks): with nothing to check, the content must not
// be presumed to match.
func PatchMatchesContent(patch, content string) bool {
	matched, _ := PatchContentMatch(patch, content)
	return matched
}

// PatchContentMatch is PatchMatchesContent plus the tab-mangling
// verdict: a content line may match either byte-exactly or after
// ExpandLeadingTabs (Claude's structuredPatch ships tab indentation
// as two spaces per tab, so a tab-indented file never byte-matches
// its own edit diff). tabExpanded reports whether any line needed the
// expansion — callers serving content lines next to this patch's
// lines apply the same transform so indentation stays consistent.
func PatchContentMatch(patch, content string) (matched, tabExpanded bool) {
	parsed := parsePatch(patch)
	var contentLines []string
	if content != "" {
		contentLines = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	verified := false
	for _, hunk := range parsed.hunks {
		newDocLines := strings.Split(string(hunk.newDoc), "\n")
		for _, ref := range hunk.lines {
			if ref.newDocLine < 0 || ref.newFileLine < 1 {
				continue
			}
			if ref.newFileLine > len(contentLines) || ref.newDocLine >= len(newDocLines) {
				return false, false
			}
			if got := contentLines[ref.newFileLine-1]; got != newDocLines[ref.newDocLine] {
				if ExpandLeadingTabs(got) != newDocLines[ref.newDocLine] {
					return false, false
				}
				tabExpanded = true
			}
			verified = true
		}
	}
	return verified, tabExpanded
}
