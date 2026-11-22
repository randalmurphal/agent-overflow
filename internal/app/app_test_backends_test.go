package app

import (
	"context"
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

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/testutil"
)

// The backend clients a test builds. Each pairing step gets the shared test
// budget (testutil.WaitBudget) in place of its product bound: the computer
// it pairs with runs in this test process, where a crowded machine can hold
// a step past that bound. The bounds themselves are their owners' to test.
// TestAppTestsBuildBackendClientsThroughTheirHelpers keeps every test in
// this package on these helpers.

// newTestBackends is attachedbackends.New for a test.
func newTestBackends(t *testing.T, dir, label, platform string) (*attachedbackends.Manager, error) {
	t.Helper()
	manager, err := attachedbackends.New(dir, label, platform)
	if err != nil {
		return nil, err
	}
	manager.SetPairingBound(testutil.WaitBudget(t))
	return manager, nil
}

// pairTestDevice is deviceclient.Pair for a test. Pair detaches the
// exchange from its context, so the exchange bound is its only bound.
func pairTestDevice(t *testing.T, dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Client, deviceclient.Pairing, error) {
	t.Helper()
	return deviceclient.Pair(t.Context(), dir, link, label, platform, testDeviceOptions(t, opts)...)
}

// redeemTestDevice is deviceclient.Redeem for a test, its context bounded by
// the same budget.
func redeemTestDevice(t *testing.T, dir string, link deviceclient.Link, label, platform string, opts ...deviceclient.Option) (*deviceclient.Redemption, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testutil.WaitBudget(t))
	defer cancel()
	return deviceclient.Redeem(ctx, dir, link, label, platform, testDeviceOptions(t, opts)...)
}

// openTestDevice is deviceclient.Open for a test.
func openTestDevice(t *testing.T, dir string, session deviceclient.Session, opts ...deviceclient.Option) (*deviceclient.Client, error) {
	t.Helper()
	return deviceclient.Open(dir, session, testDeviceOptions(t, opts)...)
}

// nearbyTestExchange is pairbootstrap.Exchange for a test, its client and
// context bounded by the same budget.
func nearbyTestExchange(t *testing.T, endpoint, label, platform string) (pairbootstrap.Invitation, string, error) {
	t.Helper()
	hc := pairbootstrap.NewHTTPClient(nil, testutil.WaitBudget(t))
	defer hc.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), testutil.WaitBudget(t))
	defer cancel()
	return pairbootstrap.Exchange(ctx, hc, endpoint, label, platform)
}

func testDeviceOptions(t *testing.T, opts []deviceclient.Option) []deviceclient.Option {
	return append(slices.Clip(opts), deviceclient.WithExchangeTimeout(testutil.WaitBudget(t)))
}

// The packages export these constructors to product code, so a test can
// still call one directly and get the product's bounds. This scan is the
// rule's check: it reads every test file of the package, whatever its build
// constraints.
func TestAppTestsBuildBackendClientsThroughTheirHelpers(t *testing.T) {
	t.Parallel()
	helpers := map[string]map[string]string{
		"agent-overflow/internal/attachedbackends": {"New": "newTestBackends"},
		"agent-overflow/internal/deviceclient":     {"Pair": "pairTestDevice", "Redeem": "redeemTestDevice", "Open": "openTestDevice"},
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
