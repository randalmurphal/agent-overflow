package highlight

import (
	"bytes"
	"strings"
)

// Fence scanner for live code highlighting and persisted code spans: finds fenced
// code blocks in markdown text the same way the frontend's marked
// pipeline does, so spans the server pushes line up with the code
// tokens the frontend creates.
//
// Deliberately narrower than CommonMark: only FLUSH-LEFT openers
// match. Indented openers (list-nested, blockquoted fences) make
// marked strip the indentation from the token text, which this
// scanner would have to replicate byte-for-byte to stay aligned —
// and misalignment is not an error, it just means no pushed span matches
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

// FenceStream is ScanFences over text that arrives in pieces: Write feeds the
// next piece and Finish ends the text. An open fence's content only grows.
// A line that could still become the closing fence is withheld until it
// cannot, or until Finish, so the content an open fence has received is
// always a prefix of the Source ScanFences reports for the same text. A
// closed fence's Source is its received content without the final newline.
type FenceStream struct {
	// line holds the current line's bytes while they can still decide
	// something: an opener's info string, or a line that could close the
	// open fence. It is empty once the line is known to be neither.
	line []byte
	// settled reports the current line is known to be plain text, or
	// content already given to the open fence.
	settled bool
	open    bool
	char    byte
	minLen  int
	count   int
}

// FenceStepKind names what one FenceStep did.
type FenceStepKind int

const (
	FenceOpened FenceStepKind = iota
	FenceContent
	FenceClosed
)

// FenceStep is one change to the fences: fence Index opened with Lang,
// received Text, or closed.
type FenceStep struct {
	Kind  FenceStepKind
	Index int
	Lang  string
	Text  []byte
}

// fenceLineKeep bounds the bytes of one line FenceStream keeps: an opener's
// info string, or a line of fence characters that could close the fence.
// A longer line is decided on its first fenceLineKeep bytes.
const fenceLineKeep = 4 << 10

// Write feeds the next piece of text, appending its steps to steps.
// FenceContent Text may alias p.
func (f *FenceStream) Write(p []byte, steps []FenceStep) []FenceStep {
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		segment := p
		if nl >= 0 {
			segment = p[:nl+1]
		}
		p = p[len(segment):]
		body := segment
		if nl >= 0 {
			body = segment[:len(segment)-1]
		}
		if f.open {
			steps = f.writeFenceLine(segment, body, nl >= 0, steps)
		} else {
			steps = f.writeTextLine(body, nl >= 0, steps)
		}
	}
	return steps
}

// writeFenceLine handles one piece of a line inside the open fence.
func (f *FenceStream) writeFenceLine(segment, body []byte, complete bool, steps []FenceStep) []FenceStep {
	if f.settled {
		steps = append(steps, FenceStep{Kind: FenceContent, Index: f.count - 1, Text: segment})
		if complete {
			f.settled = false
		}
		return steps
	}
	f.line = append(f.line, body...)
	if complete {
		if fenceCloser(string(f.line), f.char, f.minLen) {
			steps = append(steps, FenceStep{Kind: FenceClosed, Index: f.count - 1})
			f.open = false
		} else {
			steps = append(steps, FenceStep{Kind: FenceContent, Index: f.count - 1, Text: append(bytes.Clone(f.line), '\n')})
		}
		f.line = f.line[:0]
		return steps
	}
	if !couldCloseFence(f.line, f.char, f.minLen) || len(f.line) > fenceLineKeep {
		steps = append(steps, FenceStep{Kind: FenceContent, Index: f.count - 1, Text: bytes.Clone(f.line)})
		f.line = f.line[:0]
		f.settled = true
	}
	return steps
}

// writeTextLine handles one piece of a line outside any fence.
func (f *FenceStream) writeTextLine(body []byte, complete bool, steps []FenceStep) []FenceStep {
	if !f.settled {
		room := max(0, fenceLineKeep-len(f.line))
		f.line = append(f.line, body[:min(len(body), room)]...)
		if len(f.line) > 0 && f.line[0] != '`' && f.line[0] != '~' {
			f.settled = true
			f.line = f.line[:0]
		}
	}
	if !complete {
		return steps
	}
	if !f.settled {
		steps = f.openFence(steps)
	}
	f.line = f.line[:0]
	f.settled = false
	return steps
}

func (f *FenceStream) openFence(steps []FenceStep) []FenceStep {
	char, length, info, ok := fenceOpener(string(f.line))
	if !ok {
		return steps
	}
	f.open, f.char, f.minLen = true, char, length
	f.count++
	return append(steps, FenceStep{Kind: FenceOpened, Index: f.count - 1, Lang: infoLang(info)})
}

// Finish ends the text as ScanFences treats its last line: a closer closes
// the open fence, any other withheld text becomes its content, and an
// opener opens an empty fence that stays open.
func (f *FenceStream) Finish(steps []FenceStep) []FenceStep {
	switch {
	case f.open && !f.settled && fenceCloser(string(f.line), f.char, f.minLen):
		steps = append(steps, FenceStep{Kind: FenceClosed, Index: f.count - 1})
		f.open = false
	case f.open && !f.settled && len(f.line) > 0:
		steps = append(steps, FenceStep{Kind: FenceContent, Index: f.count - 1, Text: bytes.Clone(f.line)})
	case !f.open && !f.settled:
		steps = f.openFence(steps)
	}
	f.line = f.line[:0]
	f.settled = true
	return steps
}

// couldCloseFence reports whether line, the start of a line in a fence of
// minLen char, is a closing fence or could become one as more text arrives.
func couldCloseFence(line []byte, char byte, minLen int) bool {
	i := 0
	for i < 3 && i < len(line) && line[i] == ' ' {
		i++
	}
	run := 0
	for i < len(line) && line[i] == char {
		i++
		run++
	}
	if i == len(line) {
		return true
	}
	if run < minLen {
		return false
	}
	for ; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return false
		}
	}
	return true
}
