package highlight

import (
	"bytes"
	"strconv"
	"strings"

	"agent-overflow/internal/jstext"
	"agent-overflow/internal/stringsx"
)

// Frontend-parity content hashing. The frontend's span caches key by
// `frontend/src/lib/utils/fnv1a.ts`, whose fold internal/jstext computes,
// so anything the backend pushes for those caches to adopt (persisted
// spans, live code spans) must hash identically. Parity is pinned by test
// vectors generated from the JS implementation; changing either side
// without the other breaks adoption silently (pushed spans stop matching).
//
// Pushes carry hashes instead of text: the server never re-ships content
// (package doctrine), and a cumulative per-line hash chain lets the
// frontend verify its own copy of the text line by line. A prefix match
// colors exactly the verified lines; a mismatch degrades to the RPC path.
// Collisions are tolerable for the same reason the frontend accepts them:
// a wrong-but-valid span set for one render, corrected on the next
// content change.

// FrontendContentKey returns the exact string the frontend's
// `contentKey(s)` produces: `<UTF-16 length>:<fnv1a32 base36>`.
func FrontendContentKey(s string) string {
	hash, units := jstext.FoldUTF16(jstext.OffsetBasis, s)
	return strconv.Itoa(units) + ":" + strconv.FormatUint(uint64(hash), 36)
}

// FrontendLineHashes returns the cumulative fnv1a32 at each of s's line
// boundaries: entry i is the hash of s's first i+1 lines joined by
// '\n' (equivalently, the running hash just before the (i+1)-th
// newline; the last entry hashes the whole string). A consumer walking
// its own copy of the text line by line can find the longest shared
// line prefix by comparing running hashes, without the text ever
// crossing the wire twice.
func FrontendLineHashes(s string) []uint32 {
	hashes := make([]uint32, 0, strings.Count(s, "\n")+1)
	hash := jstext.OffsetBasis
	for {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			break
		}
		hash, _ = jstext.FoldUTF16(hash, s[:nl])
		hashes = append(hashes, hash)
		hash = jstext.FoldUnit(hash, '\n')
		s = s[nl+1:]
	}
	hash, _ = jstext.FoldUTF16(hash, s)
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
		c.hash = jstext.OffsetBasis
	}
	for {
		nl := bytes.IndexByte(text[c.folded:], '\n')
		if nl < 0 {
			break
		}
		line := text[c.folded : c.folded+nl]
		hash, _ := jstext.FoldUTF16(c.hash, string(line))
		c.done = append(c.done, hash)
		c.hash = jstext.FoldUnit(hash, '\n')
		c.folded += nl + 1
	}
	tail := text[c.folded:]
	tail = tail[:stringsx.CompleteRunePrefix(tail)]
	last, _ := jstext.FoldUTF16(c.hash, string(tail))
	out := make([]uint32, 0, max(0, len(c.done)+1-from))
	if from < len(c.done) {
		out = append(out, c.done[from:]...)
	}
	return append(out, last)
}
