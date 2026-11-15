package browser

import (
	"os"
	"testing"

	"agent-overflow/internal/orphanreaper"
)

// TestMain runs this binary as the orphan-reaper sidecar when a test's
// backend starts one, as main() does in the shipped binary, and otherwise
// runs the tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == orphanreaper.Subcommand() {
		orphanreaper.RunChild()
		return
	}
	runTests(m)
}
