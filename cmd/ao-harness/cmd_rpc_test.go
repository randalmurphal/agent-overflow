package main

import (
	"slices"
	"strings"
	"testing"
)

func methodCatalog() []string {
	return []string{
		"HarnessInfo", "HarnessListMocks", "HarnessListMethods", "HarnessSeed",
		"HarnessReset", "SendMessage", "ListThreads", "HarnessMockCommand",
	}
}

// A typo in a method name is the most common way `rpc` fails and the
// least self-explaining: the server's answer names the miss and nothing
// else.
func TestNearestMethodsAnswersARealTypo(t *testing.T) {
	for _, tc := range []struct {
		typed string
		want  string
	}{
		{"HarnessListMock", "HarnessListMocks"},  // one character short
		{"harnesslistmocks", "HarnessListMocks"}, // wrong case
		{"seed", "HarnessSeed"},                  // a substring the other way round
		{"HarnessRest", "HarnessReset"},          // transposed
		{"sendmessage", "SendMessage"},           //
	} {
		near := nearestMethods(tc.typed, methodCatalog())
		if !slices.Contains(near, tc.want) {
			t.Errorf("nearestMethods(%q) = %v, want it to include %q", tc.typed, near, tc.want)
		}
	}
}

// Only a HINT: a word with nothing in common must not drag the whole
// catalog into the error.
func TestNearestMethodsStaysQuietOnNonsense(t *testing.T) {
	if near := nearestMethods("zzzzzzzzzzzz", methodCatalog()); len(near) != 0 {
		t.Fatalf("nearestMethods on nonsense = %v, want nothing", near)
	}
}

func TestNearestMethodsIsBounded(t *testing.T) {
	names := []string{
		"HarnessA", "HarnessB", "HarnessC", "HarnessD",
		"HarnessE", "HarnessF", "HarnessG", "HarnessH",
	}
	if near := nearestMethods("Harness", names); len(near) > 5 {
		t.Fatalf("nearestMethods returned %d suggestions: %v", len(near), near)
	}
}

func TestFilterContainsIsCaseInsensitive(t *testing.T) {
	got := filterContains(methodCatalog(), "mock")
	if !slices.Contains(got, "HarnessListMocks") || !slices.Contains(got, "HarnessMockCommand") {
		t.Fatalf("filterContains(mock) = %v", got)
	}
	if slices.Contains(got, "HarnessSeed") {
		t.Fatalf("filterContains matched something it should not: %v", got)
	}
}

// `scenario set --name X` takes a library name, so `scenario validate X`
// meaning "open ./X" was a trap with a file-not-found for a diagnosis.
func TestLooksLikeLibraryNameSplitsTheTwoNamespaces(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want bool
	}{
		{"bench-burst-stream", true},
		{"./scenario.json", false},
		{"scenario.json", false},
		{"fixtures/a", false},
		{"", false},
	} {
		if got := looksLikeLibraryName(tc.arg); got != tc.want {
			t.Errorf("looksLikeLibraryName(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}

// A bare library name validates against the EMBEDDED library, and a miss
// points at the command that prints one.
func TestScenarioValidateReadsALibraryNameFromTheLibrary(t *testing.T) {
	result := validateScenarioFile("no-such-scenario-in-the-library", t.TempDir())
	if result.OK {
		t.Fatal("an unknown library name validated")
	}
	if !strings.Contains(result.Error, "scenario show") {
		t.Fatalf("the miss does not name where to read one: %q", result.Error)
	}
}
