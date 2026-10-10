package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// interactiveForgeCalls names the forge call each user-initiated path makes
// and the owner it reaches it through; an empty owner is a method on a. The forge transport ranks a call marked
// with forgeapi.WithInteractive ahead of background polls, so a path that drops
// the mark makes the user wait behind the pump. The pump's own marking is
// covered by TestPRPumpMarksOnlyRequestedPollsInteractive.
var interactiveForgeCalls = map[string]struct{ owner, call string }{
	"GetPRDetail":            {"gitCore", "GetPRDetail"},
	"ListPRReviewThreads":    {"gitCore", "ListReviewThreads"},
	"SubmitPRReview":         {"gitCore", "SubmitReview"},
	"ReplyToPRThread":        {"gitCore", "ReplyToThread"},
	"SetPRThreadResolved":    {"gitCore", "SetThreadResolved"},
	"SavePRCIJobLog":         {"gitCore", "GetCIJobLog"},
	"resolveForgeAttachment": {"gitCore", "FetchAttachment"},
	"GitCreatePR":            {"gitApplication", "CreatePR"},
	"SubscribePRUpdates":     {"", "fetchPRUpdateSnapshot"},
}

func TestUserForgeCallsAreMarkedInteractive(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatalf("read internal/app: %v", err)
	}
	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join("internal/app", name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isAppMethod(fn) || fn.Body == nil {
				continue
			}
			want, ok := interactiveForgeCalls[fn.Name.Name]
			if !ok {
				continue
			}
			found[fn.Name.Name] = true
			calls, marked := 0, 0
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isOwnerCall(call, want.owner, want.call) {
					return true
				}
				calls++
				if len(call.Args) > 0 && isInteractiveMark(call.Args[0]) {
					marked++
				}
				return true
			})
			callee := "a." + want.call
			if want.owner != "" {
				callee = "a." + want.owner + "()." + want.call
			}
			if calls == 0 {
				t.Errorf("%s: no call to %s", fn.Name.Name, callee)
			}
			if marked != calls {
				t.Errorf("%s: %s must receive forgeapi.WithInteractive(ctx) as its context", fn.Name.Name, callee)
			}
		}
	}
	for name := range interactiveForgeCalls {
		if !found[name] {
			t.Errorf("%s: method not found in internal/app", name)
		}
	}
}

// isOwnerCall reports whether call is a.<owner>().<method>(...), or
// a.<method>(...) when owner is empty.
func isOwnerCall(call *ast.CallExpr, owner, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	if owner == "" {
		recv, ok := sel.X.(*ast.Ident)
		return ok && recv.Name == "a"
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ownerSel, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && ownerSel.Sel.Name == owner
}

func isInteractiveMark(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithInteractive" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "forgeapi"
}
