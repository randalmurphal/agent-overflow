package transport

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The page cookie is honoured only where the Origin allow-list runs.
//
// Cookies are scoped by host and NOT by port, so a document served by any
// other listener on this machine has this backend's page cookie attached
// to every request it makes here. `OriginAllowed` is the whole defence,
// and wave 9 made it exact-port (internal/network.OriginPatterns) because
// this machine now also runs dev-server preview listeners on other ports
// of the same hosts.
//
// A defence that lives in each handler is a defence one new handler can
// arrive without. These two tests are the structural half and the
// behavioural half of "cannot arrive without it":
//
//   - TestEveryPageCookieReaderChecksOrigin reads the SOURCE of both
//     packages that hold a *Credential and fails on a function that reads
//     the cookie without asking the origin question in the same body.
//   - TestPageCookieReadersRefuseAnotherPortOnThisHost drives the real
//     routes with a real cookie and a preview-shaped Origin.
//
// A reader that cannot pass the first test is fixed, never listed. The
// exemptions below are the two functions that DEFINE the read.

// pageCookieReadCalls are the method names that read the page cookie.
// `Authenticate` is the one validation path (credential.go); `Exchange`
// calls it and then issues the cookie, which is strictly more.
var pageCookieReadCalls = map[string]bool{"Authenticate": true, "Exchange": true}

// pageCookieOriginExempt names the functions allowed to read the cookie
// without an origin check in the same body, and why each is not a door.
//
// Both are the DEFINITION of the read rather than a surface reachable
// from the network: they take an *http.Request from a caller that has
// already answered the origin question, and every such caller is checked
// by the test. Adding a third entry here would mean a route that reads
// the cookie for a document nothing vetted, which is the failure this
// file exists to make impossible.
var pageCookieOriginExempt = map[string]string{
	"internal/transport/credential.go:Credential.Authenticate": "the validation path itself; its callers carry the origin check",
	"internal/transport/credential.go:Credential.Exchange":     "the ticket exchange, called only from handleBootstrap and clientmode.handleBootstrap, both of which check first",
}

// pageCookieReaderPackages are the directories that hold code reading a
// *Credential: this package and the --connect stub, which serves the same
// SPA over a listener of its own with a Credential of its own.
var pageCookieReaderPackages = []string{".", "../clientmode"}

func TestEveryPageCookieReaderChecksOrigin(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	var unguarded []string

	for _, dir := range pageCookieReaderPackages {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				reads, checks := scanCredentialBody(fset, fn.Body)
				if !reads {
					continue
				}
				found++
				id := pageCookieFuncID(dir, name, fn)
				if _, exempt := pageCookieOriginExempt[id]; exempt {
					continue
				}
				if !checks {
					unguarded = append(unguarded, id)
				}
			}
		}
	}

	if found == 0 {
		t.Fatal("the page-cookie matcher found no readers at all; it has stopped matching and this gate is vacuous")
	}
	if len(unguarded) > 0 {
		sort.Strings(unguarded)
		t.Errorf("%d function(s) read the page cookie without calling OriginAllowed in the same body:\n  %s\n\n"+
			"Cookies are scoped by host and not by port, so a document on any other port of this "+
			"host has the cookie attached for it. Add the OriginAllowed check ahead of the "+
			"credential read; do not add the function to pageCookieOriginExempt.",
			len(unguarded), strings.Join(unguarded, "\n  "))
	}
}

// scanCredentialBody reports whether a function body reads the page
// cookie and whether it asks the origin question. Both are name matches
// on a method/function call, which is what the two behaviours are spelled
// as everywhere they appear.
func scanCredentialBody(fset *token.FileSet, body *ast.BlockStmt) (reads, checks bool) {
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "OriginAllowed" {
				checks = true
			}
			if pageCookieReadCalls[fun.Sel.Name] && looksLikeCredential(fset, fun.X) {
				reads = true
			}
		case *ast.Ident:
			if fun.Name == "OriginAllowed" {
				checks = true
			}
		}
		return true
	})
	return reads, checks
}

// looksLikeCredential filters `x.Authenticate(...)` down to the receiver
// shapes a *Credential is ever held under: a field or variable whose name
// mentions the credential, or the method's own receiver inside
// credential.go. Anything else with those method names belongs to some
// other type and is not this gate's business.
func looksLikeCredential(fset *token.FileSet, receiver ast.Expr) bool {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, receiver); err != nil {
		// An expression that will not print is not one this gate can
		// judge, and silently passing it would be the hole. Treat it as
		// a read so the failure names it.
		return true
	}
	text := strings.ToLower(buf.String())
	return strings.Contains(text, "cred") || text == "c" || text == "s"
}

func pageCookieFuncID(dir, file string, fn *ast.FuncDecl) string {
	pkgDir := "internal/transport"
	if dir != "." {
		pkgDir = "internal/" + filepath.Base(dir)
	}
	name := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		name = receiverTypeName(fn.Recv.List[0].Type) + "." + name
	}
	return pkgDir + "/" + file + ":" + name
}

func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return "?"
}

// TestPageCookieReadersRefuseAnotherPortOnThisHost drives every route
// that reads the cookie with a request that HOLDS the cookie and names a
// preview-shaped origin: the same host, another port. Each must refuse.
//
// The cookie carries this launch's real credential, so the refusal proves
// the origin rule ran AHEAD of the credential rather than that the
// credential was missing.
func TestPageCookieReadersRefuseAnotherPortOnThisHost(t *testing.T) {
	f, _ := newAttachedFixture(t)
	addr := f.srv.Addr()
	cookie := &http.Cookie{Name: pageCookieName(addr), Value: "test-token"}
	foreign := "http://127.0.0.1:5173"

	for _, tc := range []struct {
		name string
		path string
	}{
		{"bootstrap manifest", BootstrapPath},
		{"ws upgrade", WSPath},
		{"page url", PageURLPath},
		{"attached backend ws", AttachedWSPrefix + "mini"},
		{"attached backend manifest", AttachedBootstrapPrefix + "mini.json"},
		{"attached backend attachments", AttachedTransferPrefix + "mini/attachments/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://"+addr+tc.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.AddCookie(cookie)
			req.Header.Set("Origin", foreign)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			_, _ = readAllAndClose(resp)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s with Origin %s: status %d, want 404 — a document on another port of this host must not be answered",
					tc.path, foreign, resp.StatusCode)
			}
		})
	}
}
