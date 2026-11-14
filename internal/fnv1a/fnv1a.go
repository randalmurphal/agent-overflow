// Package fnv1a is the frontend's text hash, computed in Go: 32-bit FNV-1a
// folded over a string's UTF-16 code units (JavaScript `charCodeAt`), not
// its UTF-8 bytes, as `frontend/src/lib/utils/fnv1a.ts` folds it. Every
// backend value a client compares with its own hash of the same text uses
// this fold, so the two files are one contract.
package fnv1a

// OffsetBasis is the hash of the empty text.
const OffsetBasis uint32 = 0x811c9dc5

const prime uint32 = 0x01000193

// FoldUnit folds one UTF-16 code unit into hash.
func FoldUnit(hash uint32, unit uint16) uint32 {
	return (hash ^ uint32(unit)) * prime
}

// FoldUTF16 folds s's UTF-16 code units into hash and returns the new hash
// and the number of units folded.
//
// s is read as encoding/json delivers it to a client: ranging over a Go
// string yields U+FFFD, one unit, for each byte that does not begin a
// valid UTF-8 sequence, as encoding/json substitutes it. Folding a then b
// therefore equals folding a+b exactly when the cut between them is not
// inside a valid UTF-8 sequence.
func FoldUTF16(hash uint32, s string) (uint32, int) {
	units := 0
	for _, r := range s {
		if r < 0x10000 {
			hash = FoldUnit(hash, uint16(r))
			units++
			continue
		}
		r -= 0x10000
		hash = FoldUnit(hash, uint16(0xD800+(r>>10)))
		hash = FoldUnit(hash, uint16(0xDC00+(r&0x3FF)))
		units += 2
	}
	return hash, units
}
