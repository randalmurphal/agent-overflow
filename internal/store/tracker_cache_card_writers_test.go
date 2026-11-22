package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// cardWriteSQL are the SQL writes of a card, each with the one declaration
// allowed to hold it: its content and read time (tracker_cache_issues) and
// its sprints (tracker_cache_issue_sprints). A SET of the cards table is a
// card write when it assigns a content column (mirrorCardContent) or
// read_at.
var cardWriteSQL = []struct {
	what    string
	pattern *regexp.Regexp
	owner   string
}{
	{"an insert of cards", regexp.MustCompile(`(?is)\bINSERT\s+(OR\s+\w+\s+)?INTO\s+tracker_cache_issues\b`), "mirrorCardUpsertSQL"},
	{"an update of a card's content or read time", regexp.MustCompile(`(?is)\bUPDATE\s+tracker_cache_issues\s+SET\s+(.*?\b)?(` +
		cardColumnsPattern() + `)\s*=`), "mirrorCardReadSQL"},
	{"an insert of card sprints", regexp.MustCompile(`(?is)\bINSERT\s+(OR\s+\w+\s+)?INTO\s+tracker_cache_issue_sprints\b`), "mirrorCardSprintsJoinSQL"},
	{"a delete of card sprints", regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+tracker_cache_issue_sprints\b`), "mirrorCardSprintsLeaveSQL"},
	{"an update of card sprints", regexp.MustCompile(`(?is)\bUPDATE\s+tracker_cache_issue_sprints\b`), ""},
}

// cardColumnsPattern matches a column a card write sets.
func cardColumnsPattern() string {
	columns := []string{"read_at"}
	for _, c := range mirrorCardContent {
		columns = append(columns, c.column)
	}
	return strings.Join(columns, "|")
}

// cardStatementUses are the card statements and their parts, each with the
// one function allowed to use it: the parts are joined into the
// statements by newMirrorCardStatements, and only storeMirrorCardsTx runs
// them.
var cardStatementUses = map[string]string{
	"mirrorCardUpsertSQL":       "newMirrorCardStatements",
	"mirrorCardConflictSQL":     "newMirrorCardStatements",
	"mirrorCardSprintsLeaveSQL": "newMirrorCardStatements",
	"mirrorCardSprintsJoinSQL":  "newMirrorCardStatements",
	"mirrorCardReadSQL":         "storeMirrorCardsTx",
}

// cardStatementFields are the statements' fields (mirrorCardStatements),
// which only storeMirrorCardsTx reads.
var cardStatementFields = []string{"upsert", "leave", "join"}

// Every write of a card goes through the one rule that orders a card's
// writes (mirrorCardSupersededSQL): no SQL of the package, its migrations'
// triggers included, writes a card's content, read time or sprints outside
// the card statements, and nothing runs those statements but
// storeMirrorCardsTx, whose callers are the card writers. A new card writer
// calls storeMirrorCardsTx; Go cannot make the statements private to it
// inside the package, so the source is checked.
func TestCardWritesGoThroughTheOrderingRule(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	var writers []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			for owner, node := range declOwners(decl) {
				ast.Inspect(node, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind != token.STRING {
							return true
						}
						text, err := strconv.Unquote(n.Value)
						if err != nil {
							t.Fatalf("%s: %v", fset.Position(n.Pos()), err)
						}
						for _, w := range cardWriteSQL {
							if w.pattern.MatchString(text) {
								found[w.owner]++
								if owner != w.owner {
									t.Errorf("%s: %s writes %s outside %s", fset.Position(n.Pos()), owner, w.what, w.owner)
								}
							}
						}
					case *ast.Ident:
						if allowed, ok := cardStatementUses[n.Name]; ok && owner != allowed && owner != n.Name {
							t.Errorf("%s: %s uses %s, which only %s may", fset.Position(n.Pos()), owner, n.Name, allowed)
						}
					case *ast.SelectorExpr:
						if slices.Contains(cardStatementFields, n.Sel.Name) && owner != "storeMirrorCardsTx" {
							t.Errorf("%s: %s reads the card statement %s, which only storeMirrorCardsTx may", fset.Position(n.Pos()), owner, n.Sel.Name)
						}
					case *ast.CallExpr:
						if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "storeMirrorCardsTx" {
							writers = append(writers, owner)
						}
					}
					return true
				})
			}
		}
	}
	for _, w := range cardWriteSQL {
		if w.owner != "" && found[w.owner] != 1 {
			t.Errorf("%s holds %s %d times; want once", w.owner, w.what, found[w.owner])
		}
	}
	// The card writers: a board's rows, and cards read with no board,
	// which a detail document's card is.
	slices.Sort(writers)
	if want := []string{"storeLinkedCardsTx", "writeMirrorRowsTx"}; !slices.Equal(writers, want) {
		t.Errorf("storeMirrorCardsTx is called by %v; want %v", writers, want)
	}
}

// declOwners are the nodes of decl by the name that owns each: a
// function's body by the function, a value's by the value (the first
// name of its spec), a type's by the type.
func declOwners(decl ast.Decl) map[string]ast.Node {
	out := map[string]ast.Node{}
	switch d := decl.(type) {
	case *ast.FuncDecl:
		out[d.Name.Name] = d
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				out[s.Names[0].Name] = s
			case *ast.TypeSpec:
				out[s.Name.Name] = s
			}
		}
	}
	return out
}
