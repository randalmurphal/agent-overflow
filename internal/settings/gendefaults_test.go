package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every provider text generation runs on names the family a blank model
// setting runs the newest of, and the frontend reads that same map from the
// generated module rather than a copy of its own.
func TestEveryTextGenerationProviderHasADefaultFamily(t *testing.T) {
	for name := range allowedTextGenerationProviders {
		if family, ok := TextGenerationDefaultFamily(name); !ok || family == "" {
			t.Errorf("text generation provider %s has no default family", name)
		}
	}
	if _, ok := TextGenerationDefaultFamily("claude-tui"); ok {
		t.Error("claude-tui has a default family, but text generation never runs on it")
	}
	if want := `export const TEXT_GENERATION_DEFAULT_FAMILIES: Readonly<Record<string, string>> = {"agy":"gemini-4-hydrogen","claude":"claude-haiku","codex":"gpt-luna"};`; !strings.Contains(FrontendDefaultsSource(), want) {
		t.Errorf("the generated module does not carry %s", want)
	}
}

// The generated module is checked in so the frontend build needs no Go
// toolchain. This is the tripwire that keeps it honest.
func TestFrontendDefaultsSourceIsCheckedIn(t *testing.T) {
	path := filepath.FromSlash(FrontendDefaultsRelPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with `%s`)", path, err, FrontendDefaultsRegenCommand)
	}
	want := FrontendDefaultsSource()
	if string(got) != want {
		t.Fatalf("%s is stale; regenerate with `%s`", path, FrontendDefaultsRegenCommand)
	}
}

// Every deny-list entry names a real Settings json field. The generator
// emits every field the list does not deny, so a stale entry is the one
// way the list can drift from the struct.
func TestFrontendDefaultsDenyListIsTotal(t *testing.T) {
	known := knownSettingsFieldNames()
	for name := range frontendDefaultsDenied {
		if _, ok := known[name]; !ok {
			t.Errorf("deny-list names %q, which is not a Settings json field", name)
		}
	}
}
