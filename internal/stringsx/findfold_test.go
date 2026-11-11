package stringsx

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FindVectors are the shared find vectors; the frontend's findMatch.test.ts
// reads the same file.
type FindVectors struct {
	Fold  [][2]string `json:"fold"`
	Count []struct {
		Text  string `json:"text"`
		Query string `json:"query"`
		Count int    `json:"count"`
	} `json:"count"`
	Offsets []struct {
		Text  string `json:"text"`
		Query string `json:"query"`
		At    []int  `json:"at"`
	} `json:"offsets"`
}

func readFindVectors(t *testing.T) FindVectors {
	t.Helper()
	raw, err := os.ReadFile("../../frontend/src/test/fixtures/findVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v FindVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Fold) == 0 || len(v.Count) == 0 || len(v.Offsets) == 0 {
		t.Fatal("no vectors")
	}
	return v
}

func TestFoldForFindMeetsTheSharedVectors(t *testing.T) {
	t.Parallel()
	v := readFindVectors(t)
	for _, f := range v.Fold {
		got := FoldForFind(f[0])
		if got != f[1] {
			t.Errorf("FoldForFind(%q) = %q, want %q", f[0], got, f[1])
		}
		if utf8.RuneCountInString(got) != utf8.RuneCountInString(f[0]) {
			t.Errorf("FoldForFind(%q) changed the code point count", f[0])
		}
	}
	for _, c := range v.Count {
		needle := FoldForFind(strings.TrimSpace(c.Query))
		got := 0
		if needle != "" {
			got = strings.Count(FoldForFind(c.Text), needle)
		}
		if got != c.Count {
			t.Errorf("count %q in %q = %d, want %d", c.Query, c.Text, got, c.Count)
		}
	}
	for _, o := range v.Offsets {
		folded, needle := FoldForFind(o.Text), FoldForFind(strings.TrimSpace(o.Query))
		var at []int
		for from := 0; ; {
			i := strings.Index(folded[from:], needle)
			if i < 0 {
				break
			}
			at = append(at, utf8.RuneCountInString(folded[:from+i]))
			from += i + len(needle)
		}
		if !slices.Equal(at, o.At) {
			t.Errorf("offsets of %q in %q = %v, want %v", o.Query, o.Text, at, o.At)
		}
	}
}

// The frontend paints from UTF-16 offsets in its fold: a code point's lower
// case never changes its UTF-16 length.
func TestFoldForFindKeepsEveryCodePointsUTF16Length(t *testing.T) {
	t.Parallel()
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if l := unicode.ToLower(r); (r >= 0x10000) != (l >= 0x10000) {
			t.Errorf("U+%04X lowers to U+%04X across planes", r, l)
		}
	}
}

func TestFoldForFindKeepsInvalidBytes(t *testing.T) {
	t.Parallel()
	if got := FoldForFind("A\xffÉ\xfe"); got != "a\xffé\xfe" {
		t.Errorf("FoldForFind = %q", got)
	}
	if got := FoldForFind("plain"); got != "plain" {
		t.Errorf("FoldForFind = %q", got)
	}
}
