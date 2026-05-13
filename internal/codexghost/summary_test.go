package codexghost

import (
	"strings"
	"testing"
)

func TestGhostSummaryEmptyFallsBackToStandalone(t *testing.T) {
	if got := GhostSummary(""); got != "Session ended" {
		t.Errorf("GhostSummary(%q) = %q, want %q", "", got, "Session ended")
	}
	if got := GhostSummary("   "); got != "Session ended" {
		t.Errorf("GhostSummary(whitespace) = %q, want %q", got, "Session ended")
	}
}

func TestGhostSummaryAppendsSuffix(t *testing.T) {
	got := GhostSummary("Editing src/foo.go")
	want := "Editing src/foo.go" + SessionEndedSuffix
	if got != want {
		t.Errorf("GhostSummary = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, SessionEndedSuffix) {
		t.Errorf("missing suffix in %q", got)
	}
}

func TestGhostSummaryIsIdempotent(t *testing.T) {
	first := GhostSummary("Editing src/foo.go")
	second := GhostSummary(first)
	if first != second {
		t.Errorf("second pass changed result: first=%q, second=%q", first, second)
	}
}

func TestGhostSummaryTrimsLeadingTrailingWhitespace(t *testing.T) {
	got := GhostSummary("  Editing src/foo.go  ")
	want := "Editing src/foo.go" + SessionEndedSuffix
	if got != want {
		t.Errorf("GhostSummary = %q, want %q (trimmed)", got, want)
	}
}
