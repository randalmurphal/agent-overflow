package highlight

import (
	"strconv"
	"strings"
)

// Unified-diff parsing for span alignment. The response contract is
// patch-aligned: result line i corresponds to patch text line i (as
// the frontend splits it), so the frontend indexes spans by a
// PatchLine's position with zero bookkeeping. Meta lines (@@ headers,
// file headers, `\ No newline`) get plain spans.
//
// Line classification mirrors frontend patchFiles.ts exactly:
//   - inside a hunk, `+` → add, `-` → del, anything else → context
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
	oldDoc, newDoc     []byte
	lines              []hunkLineRef
}

type parsedPatch struct {
	lineCount int
	hunks     []patchHunk
}

// parsePatch parses one file's unified diff (the frontend sends one
// PatchFile's joined lines). Input outside hunks is meta by
// construction; malformed hunk headers end the current hunk rather
// than erroring — degraded output is plain spans, never a failure.
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
		result.hunks = append(result.hunks, *current)
		current = nil
		oldBody.Reset()
		newBody.Reset()
		oldLines, newLines = 0, 0
	}

	for i, line := range lines {
		if strings.HasPrefix(line, "@@") {
			finish()
			oldStart, newStart, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			current = &patchHunk{oldStart: oldStart, newStart: newStart}
			newFileLine = newStart
			continue
		}
		if current == nil {
			continue // file headers / anything before the first hunk
		}
		if isPatchMetaLine(line) {
			// A new file header inside the text ends the hunk run.
			finish()
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			newBody.WriteString(line[1:])
			newBody.WriteByte('\n')
			current.lines = append(current.lines, hunkLineRef{
				patchIndex: i, side: sideNew,
				oldDocLine: -1, newDocLine: newLines,
				newFileLine: newFileLine,
			})
			newLines++
			newFileLine++
		case strings.HasPrefix(line, "-"):
			oldBody.WriteString(line[1:])
			oldBody.WriteByte('\n')
			current.lines = append(current.lines, hunkLineRef{
				patchIndex: i, side: sideOld,
				oldDocLine: oldLines, newDocLine: -1,
				newFileLine: -1,
			})
			oldLines++
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file" — marker, not content.
		default:
			// Context. The body drops the leading space when present;
			// the output pad restores alignment with the frontend's
			// unstripped context content.
			body := line
			pad := 0
			if strings.HasPrefix(line, " ") {
				body = line[1:]
				pad = 1
			}
			oldBody.WriteString(body)
			oldBody.WriteByte('\n')
			newBody.WriteString(body)
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

// parseHunkHeader extracts the 1-based old/new start lines from
// `@@ -oldStart[,oldCount] +newStart[,newCount] @@ …`.
func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	rest := strings.TrimPrefix(line, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, false
	}
	oldStart = parseHunkNumber(fields[0][1:])
	newStart = parseHunkNumber(fields[1][1:])
	if oldStart < 0 || newStart < 0 {
		return 0, 0, false
	}
	return oldStart, newStart, true
}

func parseHunkNumber(field string) int {
	if comma := strings.IndexByte(field, ','); comma >= 0 {
		field = field[:comma]
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return -1
	}
	return n
}

// isPatchMetaLine mirrors frontend patchFiles.ts isPatchMetaLine.
func isPatchMetaLine(line string) bool {
	for _, prefix := range []string{
		"diff ", "---", "+++", "index ",
		"new file mode ", "deleted file mode ",
		"old mode ", "new mode ",
		"similarity index ", "dissimilarity index ",
		"rename from ", "rename to ",
		"copy from ", "copy to ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
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
