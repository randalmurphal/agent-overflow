package highlight

import (
	"slices"
	"testing"
)

// Vectors generated from the frontend implementation
// (frontend/src/lib/utils/fnv1a.ts semantics, run under node). These
// pin UTF-16 parity: surrogate pairs, non-ASCII, and the `<len>:<b36>`
// key format must match JS exactly or seed adoption silently stops.
func TestFrontendContentKeyParity(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "0:ztntfp"},
		{"a", "1:1r9wi7g"},
		{"abc", "3:7aigaz"},
		{"def route():\n    pass", "21:1hpft1v"},
		{"héllo wörld", "11:1mqk69y"},
		{"line1\nline2\n", "12:17oi8em"},
		{"🎉 emoji\ncafé ☕", "15:qmsjqk"},
		{"\n\n", "2:11m2iyd"},
		{"tab\there", "8:18m1kih"},
	}
	for _, tc := range cases {
		if got := FrontendContentKey(tc.in); got != tc.want {
			t.Errorf("FrontendContentKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFrontendLineHashesParity(t *testing.T) {
	cases := []struct {
		in   string
		want []uint32
	}{
		{"", []uint32{2166136261}},
		{"abc", []uint32{440920331}},
		{"def route():\n    pass", []uint32{2636444700, 3247435219}},
		{"line1\nline2\n", []uint32{2749613218, 1109853552, 2641207054}},
		{"🎉 emoji\ncafé ☕", []uint32{2112537388, 1610404076}},
		{"\n\n", []uint32{2166136261, 252472541, 2274317941}},
	}
	for _, tc := range cases {
		if got := FrontendLineHashes(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("FrontendLineHashes(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The chain property the frontend relies on: entry i equals the plain
// content hash of the first i+1 lines joined by '\n', so a consumer
// hashing its own prefix line by line lands on the same values.
func TestFrontendLineHashesArePrefixHashes(t *testing.T) {
	text := "one\ntwo\nthree"
	hashes := FrontendLineHashes(text)
	prefixes := []string{"one", "one\ntwo", "one\ntwo\nthree"}
	if len(hashes) != len(prefixes) {
		t.Fatalf("got %d hashes, want %d", len(hashes), len(prefixes))
	}
	for i, prefix := range prefixes {
		h, _ := jsHashUnits(jsFNVOffsetBasis, prefix)
		if hashes[i] != h {
			t.Errorf("hashes[%d] = %d, want prefix hash %d of %q", i, hashes[i], h, prefix)
		}
	}
}
