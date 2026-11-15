package jstext

import (
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/jstext/jstexttest"
)

// referenceShape derives a packed text's wide marks and shape from the
// packed text and the text it stands for, one character at a time.
func referenceShape(packed, text string) ([]uint32, packedShape) {
	wide := []uint32{}
	shape := packedShape{bytes: len(packed)}
	lineStart := 0
	lineWide := false
	for _, r := range packed {
		switch {
		case r == '\n':
			if lineWide {
				shape.wideBytes += shape.size - lineStart
			}
			lineWide = false
			lineStart = shape.size + 1
		case r >= 0x80 && !lineWide:
			wide = append(wide, uint32(shape.size))
			lineWide = true
		}
		shape.size++
	}
	if lineWide {
		shape.wideBytes += shape.size - lineStart
	}
	shape.lines = len(wide)
	shape.high = strings.ContainsFunc(text, func(r rune) bool { return r > 0xFF })
	return wide, shape
}

// packAlways packs raw whatever PackText's rule says of it.
func packAlways(raw string) (string, []uint32, packedShape) {
	first := asciiPrefix(raw)
	if first == len(raw) {
		return raw, []uint32{}, packedShape{size: len(raw), bytes: len(raw)}
	}
	shape := measurePacked(raw, first)
	packed, wide := pack(raw, shape)
	return packed, wide, shape
}

// checkPacked asserts the packing contract for raw, and that PackText
// answers the packed form exactly when its rule holds.
func checkPacked(t *testing.T, raw string) []uint32 {
	t.Helper()
	packed, wide, shape := packAlways(raw)
	text := jstexttest.JSONDelivered(t, raw)
	if got := jstexttest.Unpack(t, packed); got != text {
		t.Fatalf("pack(%q) unpacks to %q, JSON delivers %q", raw, got, text)
	}
	wantWide, wantShape := referenceShape(packed, text)
	if !slices.Equal(wide, wantWide) {
		t.Fatalf("pack(%q) wide = %v, want %v", raw, wide, wantWide)
	}
	if shape != wantShape {
		t.Fatalf("measurePacked(%q) = %+v, want %+v", raw, shape, wantShape)
	}
	// The bound PackText states: at most six JSON bytes per raw byte for
	// the packed text, as for the plain string, and seven with the marks
	// (the quotes and brackets aside).
	encodedPacked, _ := json.Marshal(packed)
	if len(encodedPacked) > 6*len(raw)+2 {
		t.Fatalf("packed JSON is %d bytes for %d raw bytes", len(encodedPacked), len(raw))
	}
	encodedWide, _ := json.Marshal(wide)
	if len(encodedPacked)+len(encodedWide) > 7*len(raw)+4 {
		t.Fatalf("packed JSON and marks are %d bytes for %d raw bytes", len(encodedPacked)+len(encodedWide), len(raw))
	}

	got, gotWide := PackText(raw)
	want := wantShape.high && wantShape.wideBytes*packedWideShare <= wantShape.size
	if want && (got != packed || !slices.Equal(gotWide, wide)) {
		t.Fatalf("PackText(%q) = %q %v, want %q %v", raw, got, gotWide, packed, wide)
	}
	// Packed exactly when there are marks: a client tells the forms apart
	// by them alone.
	if want != (len(gotWide) > 0) {
		t.Fatalf("PackText(%q) packs %v with %d marks", raw, want, len(gotWide))
	}
	if !want && got != raw {
		t.Fatalf("PackText(%q) left unpacked answered %q, want the input", raw, got)
	}
	return wide
}

// isPacked reports whether PackText packs raw.
func isPacked(raw string) bool {
	_, wide := PackText(raw)
	return len(wide) > 0
}

func TestPackCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		wide []uint32
	}{
		{"empty", "", []uint32{}},
		{"ascii", "diff --git a/x b/x\n+hello\n", []uint32{}},
		{"two byte", "+café\n", []uint32{4}},
		{"three byte", "a — b\n", []uint32{2}},
		{"four byte", "\U0001F600x\n", []uint32{0}},
		{"several wide lines", "aé\nplain\n—bé\n\nz\U0001F600\n", []uint32{1, 10, 19}},
		{"wide first and last line", "éfirst\nmid\nlasté", []uint32{0, 16}},
		{"begins mid-line", "é tail of a line\n+next\n", []uint32{0}},
		{"ends without newline", "+a\n+b—c", []uint32{5}},
		{"lone byte", "+caf\xe9\n", []uint32{4}},
		{"truncated before newline", "x\xe2\x82\ny", []uint32{1}},
		{"truncated four byte", "\xf0\x9f\x98", []uint32{0}},
		{"overlong", "a\xc0\xafb\n", []uint32{1}},
		{"surrogate", "a\xed\xa0\x80b\n", []uint32{1}},
		{"ff", "\xff\n\xff", []uint32{0, 4}},
		{"replacement char itself", "�\n", []uint32{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if wide := checkPacked(t, tc.raw); !slices.Equal(wide, tc.wide) {
				t.Fatalf("wide = %v, want %v", wide, tc.wide)
			}
		})
	}
}

// PackText packs only where characters past ASCII are sparse and one of
// them is past U+00FF.
func TestPackTextPacksOnlySparseWideText(t *testing.T) {
	t.Parallel()
	// 15 bytes of ASCII lines for each byte of a wide line is the limit.
	const wideLine = "+a — b" // 8 bytes
	atLimit := wideLine + "\n" + strings.Repeat("x", 8*(packedWideShare-1)-2) + "\n"
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"ascii", strings.Repeat("+plain\n", 100), false},
		{"sparse em dash", strings.Repeat("+plain line of code\n", 100) + "+a — b\n", true},
		{"sparse emoji without newline at the end", strings.Repeat("+plain line of code\n", 100) + "+done \U0001F600", true},
		{"sparse invalid byte", strings.Repeat("+plain line of code\n", 100) + "+caf\xe9\n", true},
		{"sparse latin-1 holds a byte per character already", strings.Repeat("+plain line of code\n", 100) + "+café déjà\n", false},
		{"dense latin-1", strings.Repeat("+café déjà señor\n", 100), false},
		{"dense cjk", strings.Repeat("+日本語のテキスト\n", 100), false},
		{"dense cyrillic", strings.Repeat("+строка текста\n", 100), false},
		{"one long wide line", strings.Repeat("x", 4096) + "—\n" + strings.Repeat("+plain\n", 10), false},
		{"at the limit", atLimit, true},
		{"one byte past the limit", atLimit[:len(atLimit)-2] + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkPacked(t, tc.raw)
			if ok := isPacked(tc.raw); ok != tc.want {
				t.Fatalf("PackText packed = %v, want %v", ok, tc.want)
			}
		})
	}
}

// Serial: AllocsPerRun refuses a parallel test.
func TestPackTextLeavesUnpackedTextUnallocated(t *testing.T) {
	for _, raw := range []string{strings.Repeat("+line of ascii\n", 1000), strings.Repeat("+日本語のテキスト\n", 1000)} {
		if n := testing.AllocsPerRun(10, func() { PackText(raw) }); n != 0 {
			t.Fatalf("PackText allocated %v times for text it does not pack", n)
		}
	}
}

// Serial: AllocsPerRun refuses a parallel test.
func TestPackTextSizesOutputOnce(t *testing.T) {
	raw := strings.Repeat(strings.Repeat("+plain line of code, forty bytes long ..\n", 8)+"+café — \xe9\n", 2000)
	if !isPacked(raw) {
		t.Fatal("the text was not packed")
	}
	// The builder's one buffer, which String returns, and the wide slice.
	if n := testing.AllocsPerRun(10, func() { PackText(raw) }); n > 2 {
		t.Fatalf("pack allocated %v times, want at most 2", n)
	}
}

func TestPackRandom(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"a", "\n", "\xe9", "\xc3", "\xa9", "\xe2", "\x80", "\x94", "\xf0", "\x9f", "\x98", "\x80", "\xff", "\xed", "\xa0", "é", "—", "\U0001F600", " ", "plain ascii text ", "+a line of code\n"}
	for range 5000 {
		var b strings.Builder
		for range rng.IntN(40) {
			if rng.IntN(8) == 0 {
				b.WriteByte(byte(rng.IntN(256)))
			} else {
				b.WriteString(alphabet[rng.IntN(len(alphabet))])
			}
		}
		checkPacked(t, b.String())
	}
}

func FuzzPack(f *testing.F) {
	for _, seed := range []string{"", "ascii\n", "café\n\xe9\n", "\xe2\x82\n", "\xf0\x9f\x98", "\xed\xa0\x80", strings.Repeat("+plain\n", 40) + "—\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		checkPacked(t, raw)
	})
}

// packedVector is one entry of the vectors the frontend's patch store
// reads too (frontend/src/lib/utils/patchParser.test.ts): the bytes of a
// chunk, the text a JSON client receives for them, and the chunk as
// ReadReviewDiffPacked answers it, with the key of the text as a whole
// patch.
type packedVector struct {
	Name   string   `json:"name"`
	RawHex string   `json:"rawHex"`
	Text   string   `json:"text"`
	Packed bool     `json:"packed"`
	Data   string   `json:"data"`
	Wide   []uint32 `json:"wide"`
	Key    *struct {
		Hash  uint32 `json:"hash"`
		Units int    `json:"units"`
	} `json:"key"`
}

const packedVectorsPath = "../../frontend/src/test/fixtures/packedTextVectors.json"

func TestPackTextMatchesTheSharedVectors(t *testing.T) {
	t.Parallel()
	buf, err := os.ReadFile(packedVectorsPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors []packedVector
	if err := json.Unmarshal(buf, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	sawPacked, sawPlain := false, false
	for _, v := range vectors {
		raw, err := hex.DecodeString(v.RawHex)
		if err != nil {
			t.Fatalf("%s: rawHex: %v", v.Name, err)
		}
		if got := jstexttest.JSONDelivered(t, string(raw)); got != v.Text {
			t.Errorf("%s: a JSON client receives %q, the vector says %q", v.Name, got, v.Text)
		}
		data, wide := PackText(string(raw))
		ok := len(wide) > 0
		// The vector's data is the chunk's data as a client receives it.
		if got := jstexttest.JSONDelivered(t, data); ok != v.Packed || got != v.Data || !slices.Equal(wide, v.Wide) {
			t.Errorf("%s: PackText = packed %v %q %v, the vector says packed %v %q %v", v.Name, ok, got, wide, v.Packed, v.Data, v.Wide)
		}
		hash, units := FoldUTF16(OffsetBasis, string(raw))
		if v.Key == nil || v.Key.Hash != hash || v.Key.Units != units {
			t.Errorf("%s: the bytes fold to hash %d over %d units, the vector's key is %+v", v.Name, hash, units, v.Key)
		}
		sawPacked = sawPacked || v.Packed
		sawPlain = sawPlain || !v.Packed
	}
	if !sawPacked || !sawPlain {
		t.Fatalf("vectors cover packed %v and unpacked %v chunks; want both", sawPacked, sawPlain)
	}
}
