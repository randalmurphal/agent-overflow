package webview2host

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The regression this guards is the one that cost most of the 2026-08-31
// spike: a variable that is SET BUT EMPTY still overrides the API
// argument, collapsing every environment onto one shared profile with no
// error anywhere. A presence check written as `value != ""` would let it
// through, so the test asserts the empty case specifically.
func TestScrubEnvOverridesRemovesSetButEmptyVariables(t *testing.T) {
	t.Setenv("WEBVIEW2_USER_DATA_FOLDER", "")
	t.Setenv("WEBVIEW2_RELEASE_CHANNEL_PREFERENCE", "0")

	removed := ScrubEnvOverrides()

	if !slices.Contains(removed, "WEBVIEW2_USER_DATA_FOLDER=") {
		t.Fatalf("removed = %v, want the empty WEBVIEW2_USER_DATA_FOLDER reported", removed)
	}
	if !slices.Contains(removed, "WEBVIEW2_RELEASE_CHANNEL_PREFERENCE=0") {
		t.Fatalf("removed = %v, want WEBVIEW2_RELEASE_CHANNEL_PREFERENCE reported", removed)
	}
	for _, name := range EnvOverrideNames {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("%s survived the scrub", name)
		}
	}
}

func TestScrubEnvOverridesIsQuietWhenNothingIsSet(t *testing.T) {
	for _, name := range EnvOverrideNames {
		if _, present := os.LookupEnv(name); present {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	if removed := ScrubEnvOverrides(); len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if got := FormatScrub(nil); got != "none" {
		t.Fatalf("FormatScrub(nil) = %q, want %q", got, "none")
	}
}

func TestScrubEnvOverridesIsIdempotent(t *testing.T) {
	t.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--mute-audio")
	if removed := ScrubEnvOverrides(); len(removed) == 0 {
		t.Fatal("first scrub removed nothing")
	}
	if removed := ScrubEnvOverrides(); len(removed) != 0 {
		t.Fatalf("second scrub removed %v, want none", removed)
	}
}

// The launcher logs the scrub result verbatim, so it must not be able to
// span lines and forge a log entry.
func TestScrubEnvOverridesStripsControlCharactersFromTheValue(t *testing.T) {
	t.Setenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER", "C:\\x\nfake log line")
	got := FormatScrub(ScrubEnvOverrides())
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("scrub report is multi-line and could forge a log entry: %q", got)
	}
}
