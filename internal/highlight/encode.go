package highlight

// EncodedLine is one line's spans as flat run-length pairs:
// [len0, class0, len1, class1, ...] where lengths are BYTE counts
// summing to the line's byte length. Empty/nil Runs = whole line
// plain. The frontend slices its own UTF-8 copy of the line by these
// byte lengths, so the server never re-ships text.
type EncodedLine struct {
	Runs []uint16 `json:"r,omitempty"`
}

// encodeLines run-length-encodes a per-byte class buffer into per-line
// runs. src and classes must be the same length; lines are split on
// '\n' (the newline byte itself is not part of any run). Lines longer
// than maxLineBytes, or lines whose bytes are all ClassNone, encode as
// nil (plain) — nil is the common case and keeps the wire payload
// proportional to *styled* content.
func encodeLines(src []byte, classes []uint16) []EncodedLine {
	lines := make([]EncodedLine, 0, 64)
	start := 0
	for i := 0; i <= len(src); i++ {
		if i != len(src) && src[i] != '\n' {
			continue
		}
		lines = append(lines, encodeLine(classes[start:i]))
		start = i + 1
	}
	// A trailing newline yields a final empty segment above, matching
	// strings.Split semantics on the frontend — no correction needed.
	return lines
}

func encodeLine(classes []uint16) EncodedLine {
	if len(classes) > maxLineBytes {
		return EncodedLine{}
	}
	styled := false
	for _, c := range classes {
		if c != ClassNone {
			styled = true
			break
		}
	}
	if !styled {
		return EncodedLine{}
	}
	runs := make([]uint16, 0, 8)
	runStart := 0
	for i := 1; i <= len(classes); i++ {
		if i != len(classes) && classes[i] == classes[runStart] {
			continue
		}
		runs = append(runs, uint16(i-runStart), classes[runStart])
		runStart = i
	}
	return EncodedLine{Runs: runs}
}

// plainLines returns n plain EncodedLines — the shape every failure
// and unknown-language path degrades to.
func plainLines(n int) []EncodedLine {
	return make([]EncodedLine, n)
}

// countLines matches the frontend's `split('\n').length` for the same
// text: an empty input is one line, a trailing newline adds one.
func countLines(src []byte) int {
	n := 1
	for _, b := range src {
		if b == '\n' {
			n++
		}
	}
	return n
}
