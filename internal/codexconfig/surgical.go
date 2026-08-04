package codexconfig

import (
	"bytes"
	"fmt"
	"strings"
)

// findSectionByName locates the byte range of the [mcp_servers.<name>]
// table inside data. The returned [start, end) range covers the
// header line + every line that belongs to that section (up to but
// not including the next table header, or EOF). Returns (-1, -1, nil)
// if not present. A best-effort tolerance for dotted subsections is
// included: `[mcp_servers.<name>.<sub>]` headers nested under the
// same name belong to the same logical section, so they're absorbed
// into the range as well. This keeps Update/Delete atomic against
// servers that opted into the dotted-subtable form (e.g. headers via
// `[mcp_servers.foo.http_headers]` instead of inline).
//
// Comments preceding the header are NOT absorbed into the range —
// they belong to whatever came before. AO removes only the section
// proper.
func findSectionByName(data []byte, name string) (int, int, error) {
	// Header patterns we consider belonging to this server's section.
	// Bare-key form only; users with quoted names go through a
	// different code path that's currently rejected by validate.
	prefixExact := []byte(fmt.Sprintf("[mcp_servers.%s]", name))
	prefixDotted := []byte(fmt.Sprintf("[mcp_servers.%s.", name))

	pos := 0
	start := -1
	inSection := false
	for pos < len(data) {
		lineEnd := bytes.IndexByte(data[pos:], '\n')
		var line []byte
		var advance int
		if lineEnd < 0 {
			line = data[pos:]
			advance = len(line)
		} else {
			line = data[pos : pos+lineEnd+1]
			advance = lineEnd + 1
		}
		trimmed := bytes.TrimLeft(line, " \t")

		// A new table header at the start of this line ends the
		// current section unless the header still belongs to it
		// (e.g. `[mcp_servers.<name>.subkey]`).
		if len(trimmed) > 0 && trimmed[0] == '[' {
			isOurExact := bytes.HasPrefix(trimmed, prefixExact)
			isOurDotted := bytes.HasPrefix(trimmed, prefixDotted)
			if inSection && !isOurExact && !isOurDotted {
				return start, pos, nil
			}
			if !inSection && (isOurExact || isOurDotted) {
				inSection = true
				start = pos
			}
		}
		pos += advance
	}
	if inSection {
		return start, len(data), nil
	}
	return -1, -1, nil
}

// spliceReplace returns data with the [start, end) byte range
// replaced by replacement. A nil replacement deletes the range; in
// that case we also consume the immediately-following blank line so
// successive deletes don't leave an accumulating gap.
func spliceReplace(data []byte, start, end int, replacement []byte) []byte {
	if replacement == nil {
		// Eat the blank line that often follows a section so
		// repeated deletes don't accumulate blank lines.
		for end < len(data) && (data[end] == '\n' || data[end] == '\r') {
			if data[end] == '\n' {
				end++
				break
			}
			end++
		}
	}
	out := make([]byte, 0, len(data)-(end-start)+len(replacement))
	out = append(out, data[:start]...)
	if len(replacement) > 0 {
		// Ensure a blank-line separator between the new section and
		// what follows. We add one '\n' if the replacement doesn't
		// already end on a blank line AND there's more content
		// behind.
		out = append(out, replacement...)
		if !sectionEndsWithNewline(replacement) {
			out = append(out, '\n')
		}
		if end < len(data) && !startsWithBlankLine(data[end:]) {
			out = append(out, '\n')
		}
	}
	out = append(out, data[end:]...)
	return out
}

func endsWithBlankLine(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	// A "blank line" at the tail is one of: ends with \n\n, or ends
	// with a single \n while the preceding content was just whitespace.
	if bytes.HasSuffix(b, []byte("\n\n")) {
		return true
	}
	if b[len(b)-1] == '\n' && len(b) > 1 {
		// Find the previous newline; the line between them must be
		// entirely whitespace.
		end := len(b) - 1
		prev := bytes.LastIndexByte(b[:end], '\n')
		segment := b[prev+1 : end]
		if len(strings.TrimSpace(string(segment))) == 0 {
			return true
		}
	}
	return false
}

func startsWithBlankLine(b []byte) bool {
	return len(b) > 0 && b[0] == '\n'
}

func sectionEndsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}
