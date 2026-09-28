// Package unidiff reads the structure of unified diff text: hunk headers
// and where each hunk's body ends.
//
// A body line's text can begin with anything after its one-character
// prefix, so a removed "-- note" is "--- note" and an added "++x" is
// "+++x". A line is a file header only outside a hunk body, and the
// counts in the hunk header say where the body ends. The frontend twin is
// `HunkBody` in frontend/src/lib/utils/patchFiles.ts; both read every line
// the same way.
package unidiff

import (
	"strconv"
	"strings"
)

// Hunk is a parsed `@@ -oldStart[,oldCount] +newStart[,newCount] @@`
// header. An omitted count is 1.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
}

// ParseHunkHeader parses a two-way hunk header. It reports false for any
// other line, including a combined diff's `@@@` header.
func ParseHunkHeader(line string) (Hunk, bool) {
	rest, ok := strings.CutPrefix(line, "@@ ")
	if !ok {
		return Hunk{}, false
	}
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return Hunk{}, false
	}
	oldStart, oldCount, ok := parseRange(fields[0][1:])
	if !ok {
		return Hunk{}, false
	}
	newStart, newCount, ok := parseRange(fields[1][1:])
	if !ok {
		return Hunk{}, false
	}
	return Hunk{OldStart: oldStart, OldCount: oldCount, NewStart: newStart, NewCount: newCount}, true
}

func parseRange(field string) (start, count int, ok bool) {
	startText, countText, hasCount := strings.Cut(field, ",")
	start, err := strconv.Atoi(startText)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if !hasCount {
		return start, 1, true
	}
	count, err = strconv.Atoi(countText)
	if err != nil || count < 0 {
		return 0, 0, false
	}
	return start, count, true
}

// LineKind is what a line is in a patch.
type LineKind uint8

const (
	// Outside is a line outside every hunk body: a file header, a line
	// before the first hunk, or text past a body the header's counts
	// ended. Callers read it by its prefix.
	Outside LineKind = iota
	// HunkHeader is a hunk's `@@` line, well-formed or not.
	HunkHeader
	Context
	Added
	Removed
	// NoNewline is git's `\ No newline at end of file` annotation on the
	// line above; it belongs to neither side.
	NoNewline
)

// Body follows a patch line by line and classifies each line. The zero
// value is outside any hunk.
type Body struct {
	oldLeft, newLeft int
}

// Next classifies the patch's next line. A hunk header starts a body of
// the lines its counts name; a line the body cannot hold ends it. A
// malformed header starts no body.
func (b *Body) Next(line string) LineKind {
	if strings.HasPrefix(line, "@@") {
		b.oldLeft, b.newLeft = 0, 0
		if hunk, ok := ParseHunkHeader(line); ok {
			b.oldLeft, b.newLeft = hunk.OldCount, hunk.NewCount
		}
		return HunkHeader
	}
	if strings.HasPrefix(line, `\`) {
		return NoNewline
	}
	if b.oldLeft == 0 && b.newLeft == 0 {
		return Outside
	}
	switch {
	case strings.HasPrefix(line, "+") && b.newLeft > 0:
		b.newLeft--
		return Added
	case strings.HasPrefix(line, "-") && b.oldLeft > 0:
		b.oldLeft--
		return Removed
	case (line == "" || line[0] == ' ') && b.oldLeft > 0 && b.newLeft > 0:
		// An empty line is a context line whose trailing space was
		// stripped on the way.
		b.oldLeft--
		b.newLeft--
		return Context
	}
	b.oldLeft, b.newLeft = 0, 0
	return Outside
}
