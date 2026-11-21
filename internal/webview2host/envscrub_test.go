package webview2host

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// A variable that is set but empty still overrides the API
// argument, collapsing every environment onto one shared profile with no
// error anywhere. A presence check written as `value != ""` would let it
// through, so the test asserts the empty case specifically.
func TestScrubEnvOverridesRemovesSetButEmptyVariables(t *testing.T) {
	t.Setenv("WEBVIEW2_USER_DATA_FOLDER", "")
	t.Setenv("WEBVIEW2_RELEASE_CHANNEL_PREFERENCE", "0")

	removed, err := ScrubEnvOverrides()
	if err != nil {
		t.Fatal(err)
	}

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
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	if removed, err := ScrubEnvOverrides(); err != nil || len(removed) != 0 {
		t.Fatalf("removed = %v (%v), want none", removed, err)
	}
	if got := FormatScrub(nil); got != "none" {
		t.Fatalf("FormatScrub(nil) = %q, want %q", got, "none")
	}
}

func TestScrubEnvOverridesIsIdempotent(t *testing.T) {
	t.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--mute-audio")
	if removed, err := ScrubEnvOverrides(); err != nil || len(removed) == 0 {
		t.Fatalf("first scrub removed %v (%v), want the variable", removed, err)
	}
	if removed, err := ScrubEnvOverrides(); err != nil || len(removed) != 0 {
		t.Fatalf("second scrub removed %v (%v), want none", removed, err)
	}
}

// The launcher logs the scrub result verbatim, so it must not be able to
// span lines and forge a log entry.
func TestScrubEnvOverridesStripsControlCharactersFromTheValue(t *testing.T) {
	t.Setenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER", "C:\\x\nfake log line")
	removed, err := ScrubEnvOverrides()
	if err != nil {
		t.Fatal(err)
	}
	got := FormatScrub(removed)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("scrub report is multi-line and could forge a log entry: %q", got)
	}
}
