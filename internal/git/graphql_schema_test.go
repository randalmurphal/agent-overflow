package git

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/git/forgeschema"
)

// Every GraphQL document this package can send is valid against the
// forge's schema (forgeschema, refreshed by `make forge-schemas`) and
// selects nothing it deprecates. The documents are read from the
// package's source: each package-level const or var whose value is a
// constant expression opening with an operation is a document of the
// forge its name starts with (github, gitlab). A document assembled at
// run time comes from a builder listed in builtGraphQLDocuments, and a
// literal that opens an operation anywhere else fails the test, so no
// document escapes the check.

// builtGraphQLDocuments are the documents the functions named build at
// run time, by forge, under a name for messages.
var builtGraphQLDocuments = map[string]func() []graphQLDocument{
	"gitlabPullsQuery": func() []graphQLDocument {
		var out []graphQLDocument
		for _, read := range gitlabPullsReads {
			out = append(out, graphQLDocument{Name: "gitlabPullsReads " + read.operation, Forge: "gitlab", Text: read.query})
		}
		return out
	},
}

// allowedDeprecatedGraphQL are the deprecated schema elements a document
// may select, keyed by forge and element, each with why. GitLab marks an
// experiment deprecated with a reason starting "Status: Experiment"; no
// other deprecation is allowed.
var allowedDeprecatedGraphQL = map[string]string{
	"gitlab MergeRequest.mergeabilityChecks": "the Details read's change-request check (PRManageChecks); GitLab has no stable field for it",
}

// graphQLDocument is one document and the forge it is sent to.
type graphQLDocument struct {
	Name, Forge, Text string
}

// graphQLOperationStart is a literal that opens an operation.
var graphQLOperationStart = regexp.MustCompile(`^\s*(query|mutation|subscription)[\s({]`)

// graphQLDocuments reads every document dir's non-test source defines:
// the constant ones by folding each package-level const and var whose
// value is string literals and other such names joined by +, and the
// built ones from builtGraphQLDocuments. A literal opening an operation
// outside both is an error.
func graphQLDocuments(dir string) ([]graphQLDocument, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	values := map[string]ast.Expr{}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, file)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Names) == len(vs.Values) {
					for i, n := range vs.Names {
						values[n.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	var fold func(ast.Expr, int) (string, bool)
	fold = func(e ast.Expr, depth int) (string, bool) {
		if depth > 64 {
			return "", false
		}
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		case *ast.ParenExpr:
			return fold(e.X, depth+1)
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return "", false
			}
			x, ok := fold(e.X, depth+1)
			if !ok {
				return "", false
			}
			y, ok := fold(e.Y, depth+1)
			return x + y, ok
		case *ast.Ident:
			if v, ok := values[e.Name]; ok {
				return fold(v, depth+1)
			}
		}
		return "", false
	}
	var docs []graphQLDocument
	covered := map[ast.Expr]bool{}
	for name, expr := range values {
		text, ok := fold(expr, 0)
		if !ok || !graphQLOperationStart.MatchString(text) {
			continue
		}
		forge := name[:min(len(name), len("github"))]
		if forge != "github" && forge != "gitlab" {
			return nil, fmt.Errorf("document %s names no forge: start its name with github or gitlab", name)
		}
		docs = append(docs, graphQLDocument{Name: name, Forge: forge, Text: text})
		covered[expr] = true
	}
	var stray []string
	for _, file := range parsed {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				if _, built := builtGraphQLDocuments[fn.Name.Name]; built {
					continue
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ValueSpec:
					for _, v := range n.Values {
						if covered[v] {
							return false
						}
					}
				case *ast.BasicLit:
					if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && graphQLOperationStart.MatchString(s) {
						stray = append(stray, fset.Position(n.Pos()).String())
					}
				}
				return true
			})
		}
	}
	if len(stray) > 0 {
		return nil, fmt.Errorf("these literals open a GraphQL operation outside a const or var document and outside the builders in builtGraphQLDocuments: %s", strings.Join(stray, "; "))
	}
	for _, build := range builtGraphQLDocuments {
		docs = append(docs, build()...)
	}
	slices.SortFunc(docs, func(a, b graphQLDocument) int { return strings.Compare(a.Name, b.Name) })
	return docs, nil
}

func TestGraphQLDocumentsMatchForgeSchemas(t *testing.T) {
	t.Parallel()
	docs, err := graphQLDocuments(".")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, doc := range docs {
		problems, deprecated, err := forgeschema.Check(doc.Forge, doc.Text)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Errorf("%s (%s): %s", doc.Name, doc.Forge, p)
		}
		for element, reason := range deprecated {
			key := doc.Forge + " " + element
			if _, ok := allowedDeprecatedGraphQL[key]; ok && strings.HasPrefix(reason, "Status: Experiment") {
				allowed[key] = true
				continue
			}
			t.Errorf("%s (%s) selects %s, which the schema deprecates (%s): use its replacement", doc.Name, doc.Forge, element, reason)
		}
	}
	for key := range allowedDeprecatedGraphQL {
		if !allowed[key] {
			t.Errorf("allowedDeprecatedGraphQL lists %s, which no document selects as an experiment", key)
		}
	}
	// The enumeration reaches the constant, composed and built documents.
	for _, want := range []string{"githubPRTickQuery", "githubReactionTargetQuery", "githubAddReactionMutation", "gitlabManageChecksQuery", "gitlabPipelineJobsQuery", "gitlabPullsReads PullsReviewing", "gitlabPullsReads PullsAuthoredApprovals"} {
		if !slices.ContainsFunc(docs, func(d graphQLDocument) bool { return d.Name == want }) {
			t.Errorf("the source scan missed %s", want)
		}
	}
}

// The scan finds const and var documents composed of named parts, by
// their forge prefix; it refuses a document whose name has no forge and
// a literal opening an operation outside a document or a listed
// builder.
func TestGraphQLDocumentScan(t *testing.T) {
	t.Parallel()
	scan := func(src string) ([]graphQLDocument, error) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n"+src+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return graphQLDocuments(dir)
	}
	docs, err := scan("const githubPart = `{ viewer { login } }`\n\nconst githubQ = `query Q ` + (githubPart)\n\nvar gitlabM = `mutation M { x }`\n\nconst label = \"query\"")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Name, "gitlabPullsReads") {
			found = append(found, d.Forge+" "+d.Name+": "+d.Text)
		}
	}
	if want := []string{"github githubQ: query Q { viewer { login } }", "gitlab gitlabM: mutation M { x }"}; !slices.Equal(found, want) {
		t.Errorf("documents = %q, want %q", found, want)
	}
	if _, err := scan("const readQuery = `query Q { a }`"); err == nil || !strings.Contains(err.Error(), "readQuery names no forge") {
		t.Errorf("a document without a forge: %v", err)
	}
	if _, err := scan("func build(name string) string { return `query ` + name + ` { a }` }"); err == nil || !strings.Contains(err.Error(), "a.go:3:") {
		t.Errorf("a document built outside builtGraphQLDocuments: %v", err)
	}
	if _, err := scan("func gitlabPullsQuery() {}\n\nvar built = func() string { return `query ` + `Q { a }` }()"); err == nil || !strings.Contains(err.Error(), "a.go:5:") {
		t.Errorf("a literal after a listed builder: %v", err)
	}
}
