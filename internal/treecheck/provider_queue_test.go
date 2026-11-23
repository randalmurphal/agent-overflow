package treecheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// providerQueueCheck is the grep-level half of the rule the whole revert
// exists for: AO dispatches every mid-turn message with `turn/steer` and must
// NEVER write to the app-server's own queue, not even as a fallback. Two
// dispatchers for one message is how a queued prompt runs twice, hours after
// the user typed it.
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
func providerQueueCheck() goFileCheck {
	forbidden := []string{"thread/queue/add", "thread/queue/start"}
	// The mock's tripwire IS the string, and it must keep being the string —
	// asserted below rather than merely skipped.
	const tripwire = "cmd/ao-mockprovider/codex_queue.go"
	var found []string
	return goFileCheck{
		name: "NoProductionCodePathReachesTheProviderQueueWrites",
		visit: func(fset *token.FileSet, path string, file *ast.File) {
			if strings.HasSuffix(path, "_test.go") || path == tripwire {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, method := range forbidden {
					if strings.Contains(lit.Value, method) {
						found = append(found, fmt.Sprintf("%s: a string literal names %s; Agent Overflow dispatches mid-turn messages with turn/steer and must never write to the provider's queue",
							fset.Position(lit.Pos()), method))
					}
				}
				return true
			})
		},
		report: func(t *testing.T) {
			for _, finding := range found {
				t.Error(finding)
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
		},
	}
}
