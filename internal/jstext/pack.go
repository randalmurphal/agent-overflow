package jstext

import (
	"strings"
	"unicode/utf8"
)

// packedWideShare bounds what a client decodes: text is packed only when
// its wide lines hold at most one byte in packedWideShare of it. Decoding
// costs a client in proportion to those bytes: measured in V8, a read of
// every line of a 2 MiB chunk takes 0.2 ms when none is wide, 0.8 ms at
// this share and 8 ms when all are.
const packedWideShare = 16

// PackText returns raw's text packed one byte per character when that is
// the cheaper form for a client whose strings cost two bytes per character
// once any character is past U+00FF, with the marks of its wide lines. The
// text is packed exactly when wide is non-empty; otherwise packed is raw,
// to be sent as the text it is.
//
// Packing first sanitizes raw as encoding/json does (see the package
// rule): each byte that does not begin a valid sequence becomes U+FFFD.
// Each byte b of the sanitized UTF-8 then becomes the character U+00b, so
// decoding the packed characters' code points as UTF-8 bytes gives exactly
// the text a JSON client receives for raw.
//
// wide holds, for each line (a run between newlines) of the packed text
// that has a byte past ASCII (a wide line), the index in characters of the
// first such byte, ascending. Packing needs a byte past ASCII, so packed
// text always has a wide line.
//
// A client reads an ASCII line of packed text as it is and decodes a wide
// line each time it reads one. Packing therefore pays only where
// characters past ASCII are sparse: the text has a character past U+00FF
// (without one it already costs a byte per character), and its wide lines
// hold at most 1/packedWideShare of its bytes. Text dense in such
// characters stays as it is, which is also its smaller form when they are
// three UTF-8 bytes each. The answer depends on raw alone, so a chunk read
// again has the form it had.
//
// As JSON, packed text and its marks are at most seven bytes per raw byte.
// Packed text is at most six, as the plain string is: an ASCII byte is one
// character (at most a six-byte escape), any other byte one character of
// two bytes, and an invalid byte U+FFFD's three. A wide line holds at
// least two packed characters, so under the share rule there is at most
// one mark per 32 packed characters, at most three per 32 raw bytes, and
// a mark is at most nine bytes while the packed text is under 10^8
// characters.
func PackText(raw string) (packed string, wide []uint32) {
	first := asciiPrefix(raw)
	if first == len(raw) {
		return raw, nil
	}
	shape := measurePacked(raw, first)
	if !shape.pays() {
		return raw, nil
	}
	return pack(raw, shape)
}

// packedShape is what packing raw would produce.
type packedShape struct {
	// size is the packed text's length in characters (bytes of the
	// sanitized UTF-8) and bytes its length as a Go string, where a
	// character past ASCII is two bytes.
	size  int
	bytes int
	// lines counts the wide lines and wideBytes their bytes, newlines
	// excluded.
	lines     int
	wideBytes int
	// high reports a character past U+00FF.
	high bool
}

func (s packedShape) pays() bool {
	return s.high && s.wideBytes*packedWideShare <= s.size
}

// measurePacked sizes the packed form of raw. raw[:first] is ASCII.
func measurePacked(raw string, first int) packedShape {
	shape := packedShape{size: first, bytes: first}
	// Where the current line starts, in packed characters.
	lineStart := strings.LastIndexByte(raw[:first], '\n') + 1
	lineWide := false
	for i := first; i < len(raw); {
		c := raw[i]
		if c < utf8.RuneSelf {
			if c == '\n' {
				if lineWide {
					shape.wideBytes += shape.size - lineStart
					lineWide = false
				}
				lineStart = shape.size + 1
			}
			shape.size++
			shape.bytes++
			i++
			continue
		}
		if !lineWide {
			shape.lines++
			lineWide = true
		}
		r, n := utf8.DecodeRuneInString(raw[i:])
		if r == utf8.RuneError && n == 1 {
			n = utf8.RuneLen(utf8.RuneError)
			shape.high = true
			i++
		} else {
			shape.high = shape.high || r > 0xFF
			i += n
		}
		shape.size += n
		shape.bytes += 2 * n
	}
	if lineWide {
		shape.wideBytes += shape.size - lineStart
	}
	return shape
}

// pack packs raw, whose packed form has the given shape.
func pack(raw string, shape packedShape) (packed string, wide []uint32) {
	var b strings.Builder
	b.Grow(shape.bytes)
	wide = make([]uint32, 0, shape.lines)
	pos := 0 // index in characters of the packed text
	lineWide := false
	for i := 0; i < len(raw); {
		if c := raw[i]; c < utf8.RuneSelf {
			j := i + 1 + asciiPrefix(raw[i+1:])
			run := raw[i:j]
			b.WriteString(run)
			if lineWide && strings.IndexByte(run, '\n') >= 0 {
				lineWide = false
			}
			pos += len(run)
			i = j
			continue
		}
		if !lineWide {
			wide = append(wide, uint32(pos))
			lineWide = true
		}
		r, n := utf8.DecodeRuneInString(raw[i:])
		if r == utf8.RuneError && n == 1 {
			b.WriteString(packedReplacement)
			pos += utf8.RuneLen(utf8.RuneError)
			i++
			continue
		}
		for _, c := range []byte(raw[i : i+n]) {
			b.WriteByte(0xC0 | c>>6)
			b.WriteByte(0x80 | c&0x3F)
		}
		pos += n
		i += n
	}
	return b.String(), wide
}

// packedReplacement is U+FFFD's UTF-8 bytes EF BF BD, packed.
const packedReplacement = "ï¿½"

// asciiPrefix returns the length of s's leading run of ASCII bytes.
func asciiPrefix(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return i
		}
	}
	return len(s)
}
