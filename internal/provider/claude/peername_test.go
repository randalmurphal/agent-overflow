package claude

import (
	"strings"
	"testing"
)

// The sanitizer MIRRORS the CLI's own normalizer rather than merely
// bounding the name, because the name is an address and AO compares the
// name it recorded against the name it would send next. Every case here
// is a rule read off the 2.1.237 bundle
// (`c0 = e => ppb(Oc(e.trim())).trim()`), and the truncation cap was
// additionally confirmed live (spike 2026-08-21, a 260-character --name
// registered as exactly 200 code points).
func TestSanitizePeerSessionNameMirrorsTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain name survives", "AO Thread One", "AO Thread One"},
		{"outer whitespace trimmed", "  AO Thread One\t", "AO Thread One"},
		{"tab becomes one space", "AO\tThread", "AO Thread"},
		{"a RUN of controls collapses to ONE space", "AO\t\t\nThread", "AO Thread"},
		{"zero-width space is Cf, so it collapses too", "AO​Thread", "AO Thread"},
		{"bidi marks are Cf", "AO‮Thread", "AO Thread"},
		{"line separator U+2028", "AO Thread", "AO Thread"},
		{"paragraph separator U+2029", "AO Thread", "AO Thread"},
		{"a name of only invisibles is empty", "​\t\n", ""},
		{"empty stays empty", "", ""},
		{"inner runs collapse but real spaces do not", "AO  Thread", "AO  Thread"},
		{"non-ASCII survives", "проект/ветка", "проект/ветка"},
		{"emoji survives (and counts as one code point)", "AO 🚀 Thread", "AO 🚀 Thread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizePeerSessionName(tc.in); got != tc.want {
				t.Fatalf("SanitizePeerSessionName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// CODE POINTS, not bytes and not UTF-16 units: the CLI spreads the string
// before slicing. A byte-based cap would cut a multi-byte name short and
// a UTF-16 cap would disagree on astral characters.
func TestSanitizePeerSessionNameTruncatesInCodePoints(t *testing.T) {
	got := SanitizePeerSessionName(strings.Repeat("a", MaxPeerSessionNameRunes+60))
	if len([]rune(got)) != MaxPeerSessionNameRunes {
		t.Fatalf("ASCII truncation = %d code points, want %d", len([]rune(got)), MaxPeerSessionNameRunes)
	}

	// Four bytes each, one code point each. A byte cap would keep 50.
	got = SanitizePeerSessionName(strings.Repeat("🚀", MaxPeerSessionNameRunes+10))
	if len([]rune(got)) != MaxPeerSessionNameRunes {
		t.Fatalf("astral truncation = %d code points, want %d", len([]rune(got)), MaxPeerSessionNameRunes)
	}

	// The CLI trims AFTER truncating, so a cut landing on a space must not
	// leave a trailing blank — otherwise AO's recorded name and the
	// registered name differ by one character and every sync re-renames.
	cut := strings.Repeat("a", MaxPeerSessionNameRunes-1) + " tail"
	if got := SanitizePeerSessionName(cut); strings.HasSuffix(got, " ") {
		t.Fatalf("truncation left a trailing space: %q", got)
	}
}

// Idempotence is the property the rename skip depends on: AO stores the
// sanitized name and re-sanitizes the candidate before comparing.
func TestSanitizePeerSessionNameIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"AO Thread One",
		"  AO​Thread\t\t",
		strings.Repeat("🚀", MaxPeerSessionNameRunes+10),
		strings.Repeat("a", MaxPeerSessionNameRunes-1) + " tail",
	} {
		once := SanitizePeerSessionName(in)
		if twice := SanitizePeerSessionName(once); twice != once {
			t.Fatalf("SanitizePeerSessionName not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}
