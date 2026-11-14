package fnv1a

import (
	"encoding/json"
	"math/rand/v2"
	"testing"
	"unicode/utf8"
)

// Values from frontend/src/lib/utils/fnv1a.ts run under node.
func TestFoldUTF16MatchesTheFrontend(t *testing.T) {
	cases := []struct {
		in    string
		hash  uint32
		units int
	}{
		{"", 2166136261, 0},
		{"abc", 440920331, 3},
		{"line1", 2749613218, 5},
	}
	for _, tc := range cases {
		if hash, units := FoldUTF16(OffsetBasis, tc.in); hash != tc.hash || units != tc.units {
			t.Errorf("FoldUTF16(%q) = %d, %d; want %d, %d", tc.in, hash, units, tc.hash, tc.units)
		}
	}
}

// A character past U+FFFF is its two surrogate halves, high first.
func TestFoldUTF16FoldsAnAstralCharacterAsTwoUnits(t *testing.T) {
	want := FoldUnit(FoldUnit(OffsetBasis, 0xD83C), 0xDF89)
	if hash, units := FoldUTF16(OffsetBasis, "\U0001F389"); hash != want || units != 2 {
		t.Fatalf("FoldUTF16(U+1F389) = %d, %d; want %d, 2", hash, units, want)
	}
}

// The fold reads a string as a JSON client receives it.
func TestFoldUTF16FoldsTheTextJSONDelivers(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 2000 {
		raw := make([]byte, rng.IntN(24))
		for i := range raw {
			raw[i] = byte(rng.IntN(256))
		}
		encoded, err := json.Marshal(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		var delivered string
		if err := json.Unmarshal(encoded, &delivered); err != nil {
			t.Fatal(err)
		}
		gotHash, gotUnits := FoldUTF16(OffsetBasis, string(raw))
		wantHash, wantUnits := FoldUTF16(OffsetBasis, delivered)
		if gotHash != wantHash || gotUnits != wantUnits {
			t.Fatalf("round %d: %x folds to %d, %d; its JSON text to %d, %d", round, raw, gotHash, gotUnits, wantHash, wantUnits)
		}
	}
}

// insideValidSequence reports whether cut falls inside a valid UTF-8
// sequence of s, decoded from the start as ranging decodes it.
func insideValidSequence(s string, cut int) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if i < cut && cut < i+n && !(r == utf8.RuneError && n == 1) {
			return true
		}
		i += n
	}
	return false
}

// Folding the parts of a text equals folding the whole exactly when no cut
// is inside a valid UTF-8 sequence.
func TestFoldUTF16IsCumulativeAtCutsBetweenSequences(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	alphabet := []string{"a", "\n", "é", "€", "\U0001F600", "\xff", "\xe2\x82", "\xed\xa0\x80", "\x80"}
	splitInside := 0
	for round := range 5000 {
		var raw string
		for n := rng.IntN(12); n > 0; n-- {
			raw += alphabet[rng.IntN(len(alphabet))]
		}
		cut := rng.IntN(len(raw) + 1)
		wholeHash, wholeUnits := FoldUTF16(OffsetBasis, raw)
		hash, units := FoldUTF16(OffsetBasis, raw[:cut])
		hash, more := FoldUTF16(hash, raw[cut:])
		same := hash == wholeHash && units+more == wholeUnits
		if inside := insideValidSequence(raw, cut); same == inside {
			if inside {
				t.Fatalf("round %d: %q cut at %d inside a sequence folds as the whole", round, raw, cut)
			}
			t.Fatalf("round %d: %q cut at %d folds to %d, %d; the whole to %d, %d", round, raw, cut, hash, units+more, wholeHash, wholeUnits)
		} else if inside {
			splitInside++
		}
	}
	if splitInside == 0 {
		t.Fatal("no round cut inside a sequence")
	}
}
