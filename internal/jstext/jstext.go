// Package jstext is what a JavaScript client holds for a Go string sent as
// JSON: its UTF-16 code units, their hash as the frontend folds it
// (`frontend/src/lib/utils/fnv1a.ts`), and the packed form that a browser
// stores at one byte per character (PackText).
//
// Every function here reads a string as encoding/json delivers it: each
// byte that does not begin a valid UTF-8 sequence (utf8.DecodeRuneInString
// reports RuneError, 1) reaches the client as one U+FFFD, as encoding/json
// substitutes it, and everything else as the characters it encodes.
// Consequently, the text of a then b is the text of a+b exactly when the
// cut between them is not inside a valid UTF-8 sequence.
package jstext

// OffsetBasis is the hash of the empty text.
const OffsetBasis uint32 = 0x811c9dc5

const prime uint32 = 0x01000193

// FoldUnit folds one UTF-16 code unit into hash: 32-bit FNV-1a.
func FoldUnit(hash uint32, unit uint16) uint32 {
	return (hash ^ uint32(unit)) * prime
}

// FoldUTF16 folds the UTF-16 code units of s's text into hash and returns
// the new hash and the number of units folded. Ranging over a Go string
// yields U+FFFD for each byte that does not begin a valid sequence, which
// is the text the package rule describes.
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

// Key identifies a text as the frontend keys it: the FoldUTF16 hash of its
// units from OffsetBasis, and how many units that is. No key is the zero
// value, since the empty text's hash is OffsetBasis.
type Key struct {
	Hash  uint32 `json:"hash"`
	Units int64  `json:"units"`
}

// EmptyKey is the key of the empty text.
var EmptyKey = Key{Hash: OffsetBasis}

// Extend returns the key of k's text followed by text's. Extending at a
// cut that is not inside a valid UTF-8 sequence gives the key of the
// joined text.
func (k Key) Extend(text string) Key {
	hash, units := FoldUTF16(k.Hash, text)
	return Key{Hash: hash, Units: k.Units + int64(units)}
}
