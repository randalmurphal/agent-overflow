package jira

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The package runs no code at init: every app process links it and most
// never reach Jira, so its expressions are matchers (shapes.go) and its
// tables arrays or switches. The probe in testdata links the package and
// nothing else of this module, so its init trace is the package's own,
// free of the test variables a test binary adds.
func TestInitCost(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "initcost")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", bin, "./testdata/initcost")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build the probe: %v\n%s", err, out)
	}
	run := exec.CommandContext(t.Context(), bin)
	run.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("run the probe: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		if strings.HasPrefix(line, "init agent-overflow/internal/tracker/jira ") {
			t.Errorf("the package runs code at init: %s", strings.TrimSpace(line))
		}
	}
}

// Every kind has its rule, at its index.
func TestEveryKindHasARule(t *testing.T) {
	t.Parallel()
	if len(rules) != int(kindTeam)+1 {
		t.Fatalf("%d rules for %d kinds", len(rules), kindTeam+1)
	}
	for k, r := range rules {
		if r.what == "" || (k != int(kindUnknown)) != (r.build != nil) {
			t.Errorf("kind %d: rule %+v", k, r)
		}
	}
}
