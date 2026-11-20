package tracker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The package allocates nothing at init: every app process links it. Its
// init does run code, which the trace shows as a line: ErrInvalidInput is
// trackerapi's error, a variable of another package, copied when the
// package initializes so errors.Is matches either name. The probe in
// testdata links the package and nothing else of this module, so its init
// trace is the package's own, free of the test variables a test binary
// adds.
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
		if strings.HasPrefix(line, "init agent-overflow/internal/tracker ") && !strings.HasSuffix(line, " 0 bytes, 0 allocs") {
			t.Errorf("the package allocates at init: %s", line)
		}
	}
}
