package git

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/git/forgeschema"
	"agent-overflow/internal/testutil/reporoot"
)

// allowedDeprecatedGraphQL are the deprecated schema elements a document
// may select, keyed by forge and element, each with why. GitLab marks an
// experiment deprecated with a reason starting "Status: Experiment"; no
// other deprecation is allowed.
var allowedDeprecatedGraphQL = map[string]string{
	"gitlab MergeRequest.mergeabilityChecks": "the Details read's change-request check (PRManageChecks); GitLab has no stable field for it",
}

// Every GraphQL document this package can send is valid against its
// forge's schema (forgeschema, refreshed by `make forge-schemas`) and
// selects nothing the schema deprecates. Client.GraphQL sends only a
// forgeapi.Document, whose constructor registers it, so the registry is
// every document; the package's are package-level values, made at init.
func TestGraphQLDocumentsMatchForgeSchemas(t *testing.T) {
	t.Parallel()
	docs := forgeapi.Documents()
	allowed := map[string]bool{}
	for _, doc := range docs {
		name := doc.Forge() + " " + doc.Operation()
		// The constructor's reading of the operation agrees with a parser's.
		if op := forgeschema.OperationName(doc.Text()); op != doc.Operation() {
			t.Errorf("%s: a GraphQL parser reads operation %q", name, op)
		}
		problems, deprecated, err := forgeschema.Check(doc.Forge(), doc.Text())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Errorf("%s: %s", name, p)
		}
		for element, reason := range deprecated {
			key := doc.Forge() + " " + element
			if _, ok := allowedDeprecatedGraphQL[key]; ok && strings.HasPrefix(reason, "Status: Experiment") {
				allowed[key] = true
				continue
			}
			t.Errorf("%s selects %s, which the schema deprecates (%s): use its replacement", name, element, reason)
		}
	}
	for key := range allowedDeprecatedGraphQL {
		if !allowed[key] {
			t.Errorf("allowedDeprecatedGraphQL lists %s, which no document selects as an experiment", key)
		}
	}
	t.Logf("%d documents", len(docs))
	// The registry holds the constant documents and the built ones.
	for _, want := range []string{"github PRTick", "github MergePRMessage", "gitlab PRSetDraft", "gitlab PRMergeMessage", "github PRMergeMessage", "gitlab PullsAuthoredApprovals"} {
		if !strings.Contains(registered(docs), "\n"+want+"\n") {
			t.Errorf("the registry lacks %s", want)
		}
	}
}

func registered(docs []forgeapi.Document) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, d := range docs {
		b.WriteString(d.Forge() + " " + d.Operation() + "\n")
	}
	return b.String()
}

// initTimeDocumentBuilders are the functions that may call
// forgeapi.NewDocument: each is called from a package-level var's
// initializer, so its documents are registered at init.
var initTimeDocumentBuilders = map[string]bool{"gitlabPullsQuery": true}

// The registry TestGraphQLDocumentsMatchForgeSchemas reads holds only the
// documents made by the time it runs, so every forgeapi.NewDocument call
// in the module's non-test source must run at package init: inside a
// package-level var's initializer, or inside a listed builder that such an
// initializer calls. A document made later would be sent unchecked.
func TestGraphQLDocumentsAreMadeAtInit(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	calls := 0
	err := reporoot.Walk(t, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".claude", "frontend", "e2e", "mobile", "build", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte("NewDocument(")) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// In forgeapi itself the constructor is called unqualified.
		isNewDocument := func(call *ast.CallExpr) bool {
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				return ok && pkg.Name == "forgeapi" && fun.Sel.Name == "NewDocument"
			case *ast.Ident:
				return file.Name.Name == "forgeapi" && fun.Name == "NewDocument"
			}
			return false
		}
		calledAtInit := map[string]bool{}
		for _, decl := range file.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
				ast.Inspect(gen, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if name, ok := call.Fun.(*ast.Ident); ok {
							calledAtInit[name.Name] = true
						}
					}
					return true
				})
			}
		}
		for _, decl := range file.Decls {
			fn, inFunc := decl.(*ast.FuncDecl)
			gen, inGen := decl.(*ast.GenDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isNewDocument(call) {
					return true
				}
				calls++
				where := fset.Position(call.Pos()).String()
				switch {
				case inGen && gen.Tok == token.VAR:
				case inFunc && fn.Recv == nil && initTimeDocumentBuilders[fn.Name.Name] && calledAtInit[fn.Name.Name]:
				case inFunc && fn.Recv == nil && initTimeDocumentBuilders[fn.Name.Name]:
					t.Errorf("%s: %s is listed as an init-time builder, but no package-level var in its file calls it", where, fn.Name.Name)
				default:
					t.Errorf("%s: forgeapi.NewDocument is called outside a package-level var; make the document a package-level value so the schema check sees it", where)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < len(forgeapi.Documents())-len(gitlabPullsReads)+1 {
		t.Fatalf("found %d NewDocument calls for %d registered documents: the scan misses source", calls, len(forgeapi.Documents()))
	}
}
