package app

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/testutil"
)

// The backend clients a test builds. Each pairing step asks the time its
// run has left (testutil.StepBudget) for its bound when it starts, in place
// of its product bound: the computer it pairs with runs in this test
// process, where a crowded machine can hold a step past that bound. Nothing
// after a pairing is lengthened. The bounds themselves are their owners' to
// test. TestAppTestsBuildBackendClientsThroughTheirHelpers keeps every test
// in this package on these helpers, and
// TestTheTestHelpersGiveEveryPairingStepItsBound keeps the helpers
// delivering the bound.

// pairingFixture builds a test's backend clients, each pairing step bounded
// by what bound answers when the step starts.
type pairingFixture struct {
	t     *testing.T
	bound func() (time.Duration, error)
}

// testPairing is the fixture every test uses: the run's time left.
func testPairing(t *testing.T) pairingFixture {
	return pairingFixture{t: t, bound: testutil.StepBudget(t)}
}

// newTestBackends is attachedbackends.New for a test.
func newTestBackends(t *testing.T, dir, label, platform string) (*attachedbackends.Manager, error) {
	t.Helper()
	return testPairing(t).backends(dir, label, platform)
}

// pairTestDevice is deviceclient.Pair for a test.
func pairTestDevice(t *testing.T, dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Client, deviceclient.Pairing, error) {
	t.Helper()
	return testPairing(t).pair(dir, link, label, platform, opts...)
}

// redeemTestDevice is deviceclient.Redeem for a test.
func redeemTestDevice(t *testing.T, dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Redemption, error) {
	t.Helper()
	return testPairing(t).redeem(dir, link, label, platform, opts...)
}

// nearbyTestExchange is pairbootstrap.Exchange for a test.
func nearbyTestExchange(t *testing.T, endpoint, label, platform string) (pairbootstrap.Invitation, string, error) {
	t.Helper()
	return testPairing(t).nearby(endpoint, label, platform)
}

func (f pairingFixture) backends(dir, label, platform string) (*attachedbackends.Manager, error) {
	manager, err := attachedbackends.New(dir, label, platform)
	if err != nil {
		return nil, err
	}
	manager.SetPairingBound(f.bound)
	return manager, nil
}

// pair detaches the redemption from its context (deviceclient.Pair), so the
// redemption bound is its only bound.
func (f pairingFixture) pair(dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Client, deviceclient.Pairing, error) {
	return deviceclient.Pair(f.t.Context(), dir, link, label, platform, f.redemption(opts)...)
}

func (f pairingFixture) redeem(dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Redemption, error) {
	return deviceclient.Redeem(f.t.Context(), dir, link, label, platform, f.redemption(opts)...)
}

func (f pairingFixture) redemption(opts []deviceclient.Option) []deviceclient.Option {
	return append(slices.Clip(opts), deviceclient.WithRedemptionBound(f.bound))
}

// nearby bounds the whole exchange by its client (pairbootstrap.Exchange).
func (f pairingFixture) nearby(endpoint, label, platform string) (pairbootstrap.Invitation, string, error) {
	bound, err := f.bound()
	if err != nil {
		return pairbootstrap.Invitation{}, "", err
	}
	hc, err := pairbootstrap.NewHTTPClient(nil, bound)
	if err != nil {
		return pairbootstrap.Invitation{}, "", err
	}
	defer hc.CloseIdleConnections()
	return pairbootstrap.Exchange(f.t.Context(), hc, endpoint, label, platform)
}

// Each helper hands its pairing step the fixture's bound: given one with no
// time left, a manager's pairing, a direct pairing, a redemption and a
// nearby exchange each fail at once with its error. A real backend serves
// them, so each would otherwise go ahead.
func TestTheTestHelpersGiveEveryPairingStepItsBound(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	exhausted := errors.New("this fixture has no time left")
	none := pairingFixture{t: t, bound: func() (time.Duration, error) { return 0, exhausted }}
	backend := newPairedBackend(t)

	manager, err := none.backends(t.TempDir(), "Exhausted", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	invite, _ := backend.mintLink(t, "full")
	if _, err := manager.Add(t.Context(), invite.URL); !errors.Is(err, exhausted) {
		t.Errorf("a manager's pairing = %v, want the fixture's error", err)
	}
	_, link := backend.mintLink(t, "full")
	if _, _, err := none.pair(t.TempDir(), link, "Exhausted", "linux"); !errors.Is(err, exhausted) {
		t.Errorf("a direct pairing = %v, want the fixture's error", err)
	}
	if _, err := none.redeem(t.TempDir(), link, "Exhausted", "linux"); !errors.Is(err, exhausted) {
		t.Errorf("a redemption = %v, want the fixture's error", err)
	}
	if _, _, err := none.nearby("https://"+backend.srv.Addr(), "Exhausted", "linux"); !errors.Is(err, exhausted) {
		t.Errorf("a nearby exchange = %v, want the fixture's error", err)
	}
}

// The packages export these constructors to product code, so a test can
// still call one directly and get the product's bounds. This scan is the
// rule's check: it reads every test file of the package, whatever its build
// constraints.
func TestAppTestsBuildBackendClientsThroughTheirHelpers(t *testing.T) {
	t.Parallel()
	helpers := map[string]map[string]string{
		"agent-overflow/internal/attachedbackends": {"New": "newTestBackends"},
		"agent-overflow/internal/deviceclient":     {"Pair": "pairTestDevice", "Redeem": "redeemTestDevice"},
		"agent-overflow/internal/pairbootstrap":    {"Exchange": "nearbyTestExchange", "NewHTTPClient": "nearbyTestExchange"},
	}
	// TestMain runs this package from the repository root.
	helperFile := filepath.Join("internal", "app", "app_test_backends_test.go")
	names, err := filepath.Glob(filepath.Join("internal", "app", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, helperFile) {
		t.Fatalf("the scan read %d files, none of them %s: it is not reading this package", len(names), helperFile)
	}
	fset := token.NewFileSet()
	var found []string
	for _, name := range names {
		if name == helperFile {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		imported := map[string]map[string]string{}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			funcs, ok := helpers[importPath]
			if !ok {
				continue
			}
			local := path.Base(importPath)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == "." {
				found = append(found, fmt.Sprintf("%s: dot import of %s hides its constructors from this check", fset.Position(spec.Pos()), importPath))
				continue
			}
			imported[local] = funcs
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if helper, ok := imported[pkg.Name][selector.Sel.Name]; ok {
				found = append(found, fmt.Sprintf("%s: %s.%s; use %s", fset.Position(selector.Pos()), pkg.Name, selector.Sel.Name, helper))
			}
			return true
		})
	}
	if len(found) > 0 {
		t.Fatalf("tests build backend clients past their helpers, with the product's pairing bounds:\n%s", strings.Join(found, "\n"))
	}
}
