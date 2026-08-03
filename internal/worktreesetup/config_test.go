package worktreesetup

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAcceptsAWholeRecipe(t *testing.T) {
	if err := Validate(Config{
		Copy:    []string{".env", "config/*.local"},
		Run:     [][]string{{"pnpm", "install", "--frozen-lockfile"}},
		Timeout: "15m",
	}); err != nil {
		t.Fatalf("valid config = %v", err)
	}
	if err := Validate(Config{}); err != nil {
		t.Fatalf("zero config = %v", err)
	}
}

func TestValidateCollectsEveryProblem(t *testing.T) {
	err := Validate(Config{
		Copy:    []string{" ", ".env"},
		Run:     [][]string{{}, {"  "}, {"make", "install"}},
		Timeout: "never",
	})
	if err == nil {
		t.Fatal("invalid config validated")
	}
	for _, want := range []string{"copy[0]", "run[0]", "run[1]", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("findings %q missing %q", err, want)
		}
	}
	for _, unwanted := range []string{"copy[1]", "run[2]"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Fatalf("findings %q flagged the valid element %q", err, unwanted)
		}
	}
}

func TestValidateRefusesNonPositiveTimeouts(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s", "later"} {
		if err := Validate(Config{Timeout: timeout}); err == nil {
			t.Fatalf("timeout %q validated", timeout)
		}
	}
}

// An omitted timeout resolves to DefaultTimeout in exactly one place, so
// validation and execution cannot disagree about what "absent" means.
func TestResolveTimeoutDefaultsWhenAbsent(t *testing.T) {
	for _, authored := range []string{"", "   "} {
		got, err := ResolveTimeout(authored)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.ParseDuration(DefaultTimeout)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("ResolveTimeout(%q) = %s, want %s", authored, got, want)
		}
	}
	got, err := ResolveTimeout("90s")
	if err != nil || got != 90*time.Second {
		t.Fatalf("ResolveTimeout(90s) = %s, %v", got, err)
	}
}

func TestIsZero(t *testing.T) {
	for name, config := range map[string]Config{
		"empty":   {},
		"blanks":  {Timeout: "  "},
		"nilrun":  {Copy: nil, Run: nil},
		"empties": {Copy: []string{}, Run: [][]string{}},
	} {
		if !config.IsZero() {
			t.Fatalf("%s reported a configured recipe", name)
		}
	}
	for name, config := range map[string]Config{
		"copy":    {Copy: []string{".env"}},
		"run":     {Run: [][]string{{"make"}}},
		"timeout": {Timeout: "5m"},
	} {
		if config.IsZero() {
			t.Fatalf("%s reported an empty recipe", name)
		}
	}
}
