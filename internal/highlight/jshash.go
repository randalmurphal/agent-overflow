package highlight

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Frontend-parity content hashing. The frontend's span caches key by
// `frontend/src/lib/utils/fnv1a.ts` — 32-bit FNV-1a folded over UTF-16
// CODE UNITS (JS `charCodeAt`), not UTF-8 bytes — so anything the
// backend pushes for those caches to adopt (persisted spans, live code
// spans) must hash identically. Parity is pinned by test vectors
// generated from the JS implementation; changing either side without the
// other breaks adoption silently (pushed spans just stop matching), so
// treat the two files as one contract.
//
// Pushes carry hashes INSTEAD of text: the server never re-ships
// content (package doctrine), and a cumulative per-line hash chain
// lets the frontend verify its own copy of the text line by line — a
// prefix match colors exactly the verified lines, a mismatch degrades
// to the RPC path. Collisions are tolerable for the same reason the
// frontend accepts them: a wrong-but-valid span set for one render,
// self-corrected on the next content change.

const (
	jsFNVOffsetBasis uint32 = 0x811c9dc5
	jsFNVPrime       uint32 = 0x01000193
)

// jsHashUnits folds s's UTF-16 code units into hash, returning the new
// hash and the number of units folded.
func jsHashUnits(hash uint32, s string) (uint32, int) {
	units := 0
	for _, r := range s {
		if r < 0x10000 {
			hash = (hash ^ uint32(r)) * jsFNVPrime
			units++
			continue
		}
		r -= 0x10000
		hash = (hash ^ (0xD800 + uint32(r>>10))) * jsFNVPrime
		hash = (hash ^ (0xDC00 + uint32(r&0x3FF))) * jsFNVPrime
		units += 2
	}
	return hash, units
}

// FrontendContentKey returns the exact string the frontend's
// `contentKey(s)` produces: `<UTF-16 length>:<fnv1a32 base36>`.
func FrontendContentKey(s string) string {
	hash, units := jsHashUnits(jsFNVOffsetBasis, s)
	return strconv.Itoa(units) + ":" + strconv.FormatUint(uint64(hash), 36)
}

// FrontendLineHashes returns the cumulative fnv1a32 at each of s's line
// boundaries: entry i is the hash of s's first i+1 lines joined by
// '\n' (equivalently, the running hash just before the (i+1)-th
// newline; the last entry hashes the whole string). A consumer walking
// its own copy of the text line by line can find the longest shared
// line prefix by comparing running hashes — without the text ever
// crossing the wire twice.
func FrontendLineHashes(s string) []uint32 {
	hashes := make([]uint32, 0, strings.Count(s, "\n")+1)
	hash := jsFNVOffsetBasis
	for _, r := range s {
		if r == '\n' {
			hashes = append(hashes, hash)
			hash = (hash ^ '\n') * jsFNVPrime
			continue
		}
		if r < 0x10000 {
			hash = (hash ^ uint32(r)) * jsFNVPrime
			continue
		}
		p := r - 0x10000
		hash = (hash ^ (0xD800 + uint32(p>>10))) * jsFNVPrime
		hash = (hash ^ (0xDC00 + uint32(p&0x3FF))) * jsFNVPrime
	}
	return append(hashes, hash)
}

// LineHashChain is FrontendLineHashes over text that grows at its end. It
// folds each line once, when its newline arrives; only the last, incomplete
// line is rehashed per call.
type LineHashChain struct {
	done   []uint32 // entry per complete line
	hash   uint32   // running hash just past the last complete line's newline
	folded int      // bytes of text folded into done and hash
}

// Hashes returns FrontendLineHashes(text)[from:]. text must extend the text
// of every earlier call. A multi-byte character cut off at the end of text is
// left out of the last entry until the rest of it arrives.
func (c *LineHashChain) Hashes(text []byte, from int) []uint32 {
	if c.folded == 0 && len(c.done) == 0 {
		c.hash = jsFNVOffsetBasis
	}
	for {
		nl := bytes.IndexByte(text[c.folded:], '\n')
		if nl < 0 {
			break
		}
		line := text[c.folded : c.folded+nl]
		hash, _ := jsHashUnits(c.hash, string(line))
		c.done = append(c.done, hash)
		c.hash = (hash ^ '\n') * jsFNVPrime
		c.folded += nl + 1
	}
	tail := text[c.folded:]
	for cut := len(tail) - 1; cut >= 0 && cut >= len(tail)-utf8.UTFMax; cut-- {
		if utf8.RuneStart(tail[cut]) {
			if !utf8.FullRune(tail[cut:]) {
				tail = tail[:cut]
			}
			break
		}
	}
	last, _ := jsHashUnits(c.hash, string(tail))
	out := make([]uint32, 0, max(0, len(c.done)+1-from))
	if from < len(c.done) {
		out = append(out, c.done[from:]...)
	}
	return append(out, last)
}
