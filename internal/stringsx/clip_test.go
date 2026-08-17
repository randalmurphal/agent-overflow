package stringsx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"equal to max", "hello", 5, "hello"},
		{"longer than max truncates", "hello world", 5, "hello"},
		{"empty string", "", 5, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clip(tc.in, tc.max); got != tc.want {
				t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestClipRunes(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"equal to max", "hello", 5, "hello"},
		{"longer than max truncates", "hello world", 5, "hello"},
		{"empty string", "", 5, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
		// "é" is two bytes: a cut at 1 or 3 lands mid-rune and must back
		// off to the boundary below it.
		{"cut inside the first rune", "éa", 1, ""},
		{"cut on a rune boundary", "éa", 2, "é"},
		{"cut inside the second rune", "aéb", 2, "a"},
		{"three-byte rune backs off", "日本", 4, "日"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClipRunes(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("ClipRunes(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}

func TestTailRunes(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"equal to max", "hello", 5, "hello"},
		{"longer than max keeps the end", "hello world", 5, "world"},
		{"empty string", "", 5, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
		{"cut inside the last rune", "aé", 1, ""},
		{"cut on a rune boundary", "aé", 2, "é"},
		{"three-byte rune advances", "日本", 4, "本"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TailRunes(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("TailRunes(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}

// TestClipAndTailRunesNeverTearRunes sweeps every byte offset of a
// three-byte-per-rune string: both cuts must stay valid UTF-8 and must
// never exceed the byte budget they were given.
func TestClipAndTailRunesNeverTearRunes(t *testing.T) {
	s := strings.Repeat("日", 10)
	for n := -1; n <= len(s)+1; n++ {
		clipped := ClipRunes(s, n)
		if !utf8.ValidString(clipped) {
			t.Fatalf("ClipRunes(%d) = %q is not valid UTF-8", n, clipped)
		}
		if n < len(s) && len(clipped) > max(n, 0) {
			t.Fatalf("ClipRunes(%d) returned %d bytes", n, len(clipped))
		}
		tail := TailRunes(s, n)
		if !utf8.ValidString(tail) {
			t.Fatalf("TailRunes(%d) = %q is not valid UTF-8", n, tail)
		}
		if n < len(s) && len(tail) > max(n, 0) {
			t.Fatalf("TailRunes(%d) returned %d bytes", n, len(tail))
		}
	}
}
