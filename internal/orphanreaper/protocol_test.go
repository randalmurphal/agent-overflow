package orphanreaper

import (
	"strings"
	"testing"
)

func TestParseCommandValid(t *testing.T) {
	cases := []struct {
		line string
		want command
	}{
		{"watch 100", command{kind: cmdWatch, pgid: 100}},
		{"release 250", command{kind: cmdRelease, pgid: 250}},
		{"  watch   42  ", command{kind: cmdWatch, pgid: 42}}, // Fields tolerates padding
	}
	for _, tc := range cases {
		got, err := parseCommand(tc.line)
		if err != nil {
			t.Fatalf("parseCommand(%q) error: %v", tc.line, err)
		}
		if got != tc.want {
			t.Errorf("parseCommand(%q) = %+v, want %+v", tc.line, got, tc.want)
		}
	}
}

func TestParseCommandRejectsBad(t *testing.T) {
	bad := []string{
		"",          // empty
		"watch",     // missing pgid
		"watch 1 2", // too many fields
		"watch abc", // non-numeric
		"watch 0",   // pgid 0 == caller's own group
		"watch 1",   // pgid 1 == init
		"watch -5",  // negative
		"kill 100",  // unknown verb
		"release 1", // init
	}
	for _, line := range bad {
		if _, err := parseCommand(line); err == nil {
			t.Errorf("parseCommand(%q) = nil error, want rejection", line)
		}
	}
}

func TestFormatRoundTrips(t *testing.T) {
	if c, err := parseCommand(strings.TrimSuffix(formatWatch(77), "\n")); err != nil || c.kind != cmdWatch || c.pgid != 77 {
		t.Fatalf("watch round-trip failed: %+v err=%v", c, err)
	}
	if c, err := parseCommand(strings.TrimSuffix(formatRelease(88), "\n")); err != nil || c.kind != cmdRelease || c.pgid != 88 {
		t.Fatalf("release round-trip failed: %+v err=%v", c, err)
	}
}
