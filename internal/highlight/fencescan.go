package highlight

import "strings"

// Streaming fence scanner for the highlight seed push: finds fenced
// code blocks in markdown text the same way the frontend's marked
// pipeline does, so spans the server pushes line up with the code
// tokens the frontend creates.
//
// Deliberately narrower than CommonMark: only FLUSH-LEFT openers
// match. Indented openers (list-nested, blockquoted fences) make
// marked strip the indentation from the token text, which this
// scanner would have to replicate byte-for-byte to stay aligned —
// and misalignment is not an error, it just means no seed matches
// and the block falls back to the RPC path. Agent output fences are
// overwhelmingly flush-left, so the narrow rule keeps the hit rate
// high and the divergence surface near zero.

// Fence is one fenced code block found by ScanFences.
type Fence struct {
	// Lang is the first whitespace-delimited word of the info string,
	// exactly as marked exposes `token.lang` ("" for a bare fence).
	Lang string
	// Source is the fence content as marked exposes `token.text`: the
	// lines between opener and closer without the newline that
	// precedes the closer. For an unclosed fence it is everything
	// after the opener line, as-is.
	Source string
	// Closed reports whether the closing fence has arrived. At most
	// the LAST fence of a scan can be open.
	Closed bool
}

// ScanFences returns the fenced code blocks of text, in order.
func ScanFences(text string) []Fence {
	var fences []Fence
	var open *Fence
	fenceChar := byte(0)
	fenceLen := 0
	contentStart := 0

	pos := 0
	for pos <= len(text) {
		lineEnd := strings.IndexByte(text[pos:], '\n')
		atEOF := lineEnd < 0
		var line string
		if atEOF {
			line = text[pos:]
		} else {
			line = text[pos : pos+lineEnd]
		}

		if open == nil {
			if char, length, info, ok := fenceOpener(line); ok {
				fences = append(fences, Fence{Lang: infoLang(info)})
				open = &fences[len(fences)-1]
				fenceChar = char
				fenceLen = length
				contentStart = pos + len(line) + 1 // may be len(text)+1 at EOF
			}
		} else if fenceCloser(line, fenceChar, fenceLen) {
			// Content runs to the start of the closer line, minus the
			// newline that terminated the last content line (marked's
			// token.text carries no trailing newline).
			content := ""
			if pos > contentStart {
				content = strings.TrimSuffix(text[contentStart:pos], "\n")
			}
			open.Source = content
			open.Closed = true
			open = nil
		}

		if atEOF {
			break
		}
		pos += lineEnd + 1
	}

	if open != nil && contentStart <= len(text) {
		open.Source = text[contentStart:]
	}
	return fences
}

// fenceOpener matches a flush-left ``` / ~~~ opener (3+ fence chars).
// CommonMark forbids backticks in a backtick fence's info string; a
// line like "``` a`b ```" is inline code, not an opener.
func fenceOpener(line string) (char byte, length int, info string, ok bool) {
	if len(line) < 3 {
		return 0, 0, "", false
	}
	char = line[0]
	if char != '`' && char != '~' {
		return 0, 0, "", false
	}
	length = fenceRun(line, char)
	if length < 3 {
		return 0, 0, "", false
	}
	info = strings.TrimSpace(line[length:])
	if char == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, "", false
	}
	return char, length, info, true
}

// fenceCloser matches a closing fence: up to 3 leading spaces, then a
// run of the opener's fence char at least as long as the opener, then
// only whitespace. A shorter run (or the other fence char) is content
// — matching marked's char/length-aware close.
func fenceCloser(line string, char byte, minLen int) bool {
	trimmed := line
	for indent := 0; indent < 3 && len(trimmed) > 0 && trimmed[0] == ' '; indent++ {
		trimmed = trimmed[1:]
	}
	run := fenceRun(trimmed, char)
	if run < minLen {
		return false
	}
	return strings.TrimRight(trimmed[run:], " \t") == ""
}

func fenceRun(s string, char byte) int {
	n := 0
	for n < len(s) && s[n] == char {
		n++
	}
	return n
}

// infoLang extracts marked's `token.lang`: the first whitespace-
// delimited word of the (already trimmed) info string.
func infoLang(info string) string {
	if i := strings.IndexAny(info, " \t"); i >= 0 {
		return info[:i]
	}
	return info
}
