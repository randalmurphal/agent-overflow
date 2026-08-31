package identity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The class gate for "Revocation is absolute" (docs/specs/remote-access.md
// §2). TestNoMintPathAdmitsARevokedDevice proves the paths that exist
// today refuse a revoked device; this proves a path added TOMORROW cannot
// quietly opt out.
//
// It works because the enforcement is not in any of those callers. Four
// calls are what bring a credential into existence or keep one alive, and
// each carries the device gate at the point of the write:
//
//   - store.CreateSession — the device predicate is inside the INSERT;
//   - store.ActivateSession, store.ExtendSession — inside the UPDATE;
//   - signClaims — reached only through Mint (which must then survive
//     CreateSession) or issueFor (which takes the device ROW and refuses
//     a revoked one).
//
// So a new mint path built from these four inherits the refusal. This test
// fails when one of them is called from somewhere new, which is the moment
// somebody has to decide whether the new caller is a mint path — and if it
// is, add it to TestNoMintPathAdmitsARevokedDevice rather than only here.
//
// Widening a list below is not a fix on its own. It is the prompt to
// answer, in the same change, what stops that call producing a credential
// for a device the owner revoked.
func TestEveryCredentialProducingCallGoesThroughAChokepoint(t *testing.T) {
	chokepoints := map[string][]string{
		"CreateSession":   {"Sessions.Mint"},
		"ActivateSession": {"Sessions.ConfirmPairing"},
		"ExtendSession":   {"Sessions.EnsureLocalChannelSession", "Sessions.reissue"},
		"signClaims":      {"Sessions.Mint", "Sessions.issueFor"},
	}
	found := map[string][]string{}
	for name, callers := range callersInPackage(t) {
		if _, gated := chokepoints[name]; !gated {
			continue
		}
		found[name] = callers
	}
	for name, want := range chokepoints {
		got := found[name]
		if got == nil {
			t.Errorf("%s is called from nothing; the chokepoint it names is gone", name)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s is called from %v, want exactly %v\n"+
				"a new caller of this is a new way to produce a credential; say what stops it "+
				"doing so for a revoked device (docs/specs/remote-access.md §2)", name, got, want)
		}
	}
}

// callersInPackage maps a callee spelling to the sorted, deduplicated
// `Receiver.Method` (or bare function) names that call it, across every
// non-test file of this package.
func callersInPackage(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	callers := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			enclosing := funcName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calleeName(call.Fun)
				if callee == "" {
					return true
				}
				if callers[callee] == nil {
					callers[callee] = map[string]bool{}
				}
				callers[callee][enclosing] = true
				return true
			})
		}
	}
	out := make(map[string][]string, len(callers))
	for callee, names := range callers {
		list := make([]string, 0, len(names))
		for name := range names {
			list = append(list, name)
		}
		sort.Strings(list)
		out[callee] = list
	}
	return out
}

// funcName renders a declaration as `Receiver.Method`, or the bare name
// for a plain function.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return receiverName(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	default:
		return "?"
	}
}

// calleeName is the callee's own identifier, whatever it was reached
// through: `signClaims`, and `CreateSession` for `s.store.CreateSession`
// alike. Deliberately receiver-blind — keying on the spelling of the
// receiver would let a new caller slip past by holding the store in a
// local, which is the shape a gate is least able to afford missing.
func calleeName(fun ast.Expr) string {
	switch typed := fun.(type) {
	case *ast.SelectorExpr:
		return typed.Sel.Name
	case *ast.Ident:
		return typed.Name
	default:
		return ""
	}
}
