package stringsx

// SkipANSIEscape returns the index just past the ANSI/OSC escape sequence that
// begins at s[i] (the caller guarantees s[i] == ESC, 0x1b). It recognizes CSI
// (ESC [ ... final byte 0x40-0x7e), OSC (ESC ] ... BEL or ST), two-byte charset
// designators (ESC ( / ) X), and a bare ESC followed by one byte.
//
// An unterminated CSI or OSC — no final byte / no BEL or ST before s ends —
// returns i+1, resuming just past the ESC rather than consuming the rest of s.
// That keeps a stray ESC[ or ESC] from swallowing the bytes after it: content
// following a truncated sequence, or (for a caller that re-scans an accumulating
// buffer) a marker that only arrives in a later chunk. A sequence merely split at
// the end of s leaks its partial bytes once; the next re-scan, with the
// terminator now present, skips it cleanly.
//
// The type parameter lets a byte-oriented caller (a raw PTY scan) and a
// string-oriented one (de-ANSI'ing a command line) share one skipper without
// either converting first.
func SkipANSIEscape[T ~[]byte | ~string](s T, i int) int {
	n := len(s)
	if i+1 >= n {
		return n
	}
	switch s[i+1] {
	case '[': // CSI: parameters/intermediates until a final byte 0x40-0x7e
		j := i + 2
		for j < n && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j >= n {
			return i + 1 // unterminated: resume past ESC, don't swallow the tail
		}
		return j + 1
	case ']': // OSC: until BEL (0x07) or ST (ESC \)
		j := i + 2
		for j < n {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < n && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return i + 1 // unterminated: resume past ESC, don't swallow the tail
	case '(', ')': // charset designator: ESC ( X / ESC ) X
		// i+1 < n is guaranteed above; if X is missing (i+2 == n) the returned
		// i+3 overshoots and the caller's index loop simply ends — safe.
		return i + 3
	default:
		return i + 2
	}
}
