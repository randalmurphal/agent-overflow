package linearschema

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The package links no testing package, so non-test code can use it.
func TestNoTestingInNonTestCode(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{"agent-overflow/internal/tracker/linear/linearschema", "agent-overflow/internal/harness/trackerfake"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			var stderr string
			if exitErr, ok := err.(*exec.ExitError); ok {
				stderr = string(exitErr.Stderr)
			}
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, stderr)
		}
		if slices.Contains(strings.Fields(string(out)), "testing") {
			t.Errorf("%s links testing", pkg)
		}
	}
}
