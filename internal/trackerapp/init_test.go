package trackerapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The package allocates nothing at init: every app process links it and
// most have no tracker connection. The probe in testdata links the package
// and nothing else of this module, so its init trace is the package's own,
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
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "init agent-overflow/internal/trackerapp ") && !strings.HasSuffix(line, " 0 bytes, 0 allocs") {
			t.Errorf("the package allocates at init: %s", line)
		}
	}
}

// isLinearURLKey matches exactly what the expression it replaced does.
func TestLinearURLKeyShape(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	keys := []string{"", "a", "0", "-", "-a", "a-", "a--b", "acme", "Acme", "a b", "a_b", "a.b", "a\n", "\na", "é", "a\xff",
		strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("-", 64), "a" + strings.Repeat("-", 63)}
	for b := range 256 {
		keys = append(keys, string([]byte{byte(b)}), string([]byte{'a', byte(b)}), string([]byte{byte(b), 'a'}))
	}
	for _, k := range keys {
		if got, want := isLinearURLKey(k), shape.MatchString(k); got != want {
			t.Errorf("isLinearURLKey(%q) = %v, the expression says %v", k, got, want)
		}
	}
}
