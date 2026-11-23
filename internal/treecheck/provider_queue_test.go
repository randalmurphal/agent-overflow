package treecheck

import (
	"go/ast"
	"go/token"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// TestNoProductionCodePathReachesTheProviderQueueWrites is the grep-level half
// of the rule the whole revert exists for: AO dispatches every mid-turn message
// with `turn/steer` and must NEVER write to the app-server's own queue, not
// even as a fallback. Two dispatchers for one message is how a queued prompt
// runs twice, hours after the user typed it.
//
// The wrapper is already gone (`codex.Session` has no QueueAdd), so this is a
// guard against it growing back somewhere the compiler cannot see — a raw
// `sendRequest`, a method name assembled from a constant. It reads STRING
// LITERALS out of the AST rather than grepping text, so the many comments that
// legitimately explain the retired path do not have to be phrased around it.
//
// The harness mock is exempt by design and named here so the exemption stays
// honest: its refusal of both methods is what turns a regrown caller into a
// failing harness run. Test files are not scanned; internal/app's fake
// app-server mirrors the mock.
func TestNoProductionCodePathReachesTheProviderQueueWrites(t *testing.T) {
	t.Parallel()
	forbidden := []string{"thread/queue/add", "thread/queue/start"}
	// The mock's tripwire IS the string, and it must keep being the string —
	// asserted below rather than merely skipped.
	const tripwire = "cmd/ao-mockprovider/codex_queue.go"

	fset := token.NewFileSet()
	for _, path := range reporoot.GoFiles(t) {
		if strings.HasSuffix(path, "_test.go") || path == tripwire {
			continue
		}
		file, err := reporoot.ParseGoFile(t, fset, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, method := range forbidden {
				if strings.Contains(lit.Value, method) {
					t.Errorf("%s: a string literal names %s; Agent Overflow dispatches mid-turn messages with turn/steer and must never write to the provider's queue",
						fset.Position(lit.Pos()), method)
				}
			}
			return true
		})
	}

	raw, err := os.ReadFile(reporoot.Path(t, tripwire))
	if err != nil {
		t.Fatalf("read the mock's queue tripwire: %v", err)
	}
	for _, method := range forbidden {
		if !strings.Contains(string(raw), method) {
			t.Errorf("%s no longer refuses %s; the exemption above is now hiding nothing", tripwire, method)
		}
	}
}
