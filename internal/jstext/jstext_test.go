package jstext

import (
	"math/rand/v2"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/jstext/jstexttest"
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
		delivered := jstexttest.JSONDelivered(t, string(raw))
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
		_, n := utf8.DecodeRuneInString(s[i:])
		if i < cut && cut < i+n {
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

func TestEmptyKeyIsTheEmptyTextsAndNotZero(t *testing.T) {
	if EmptyKey != (Key{Hash: 2166136261}) || EmptyKey == (Key{}) {
		t.Fatalf("EmptyKey = %+v", EmptyKey)
	}
	if got := EmptyKey.Extend(""); got != EmptyKey {
		t.Fatalf("extending by no text = %+v", got)
	}
}

// Extending a key at each cut between sequences gives the key of the whole
// text, with units counted as the client counts them.
func TestKeyExtendIsTheJoinedTextsKey(t *testing.T) {
	text := "a é — \U0001F600 \xff \xe2\x82 end"
	hash, units := FoldUTF16(OffsetBasis, text)
	whole := Key{Hash: hash, Units: int64(units)}
	// Each invalid byte is one unit, the astral character two.
	if want := int64(len([]rune(text)) + 1); whole.Units != want {
		t.Fatalf("%q is %d units, want %d", text, whole.Units, want)
	}
	if got := EmptyKey.Extend(text); got != whole {
		t.Fatalf("EmptyKey.Extend = %+v, want %+v", got, whole)
	}
	for cut := 0; cut <= len(text); cut++ {
		if insideValidSequence(text, cut) {
			continue
		}
		if got := EmptyKey.Extend(text[:cut]).Extend(text[cut:]); got != whole {
			t.Fatalf("cut at %d: %+v, want %+v", cut, got, whole)
		}
	}
}
