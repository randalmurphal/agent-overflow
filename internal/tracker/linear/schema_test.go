package linear

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"agent-overflow/internal/tracker/linear/linearschema"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// registeredOps is every operation the package registered by name, a
// family by its largest document.
func registeredOps() map[string]operation {
	out := map[string]operation{}
	for _, op := range registry.ops[:registry.nOps] {
		out[op.name] = op
	}
	for _, f := range registry.families[:registry.nFam] {
		out[f.name] = f.op(len(f.docs))
	}
	return out
}

// allOperations is every operation the package registered, in name
// order, a name declared twice listed twice.
func allOperations() []operation {
	out := slices.Clone(registry.ops[:registry.nOps])
	for _, f := range registry.families[:registry.nFam] {
		out = append(out, f.op(len(f.docs)))
	}
	slices.SortFunc(out, func(a, b operation) int { return strings.Compare(a.name, b.name) })
	return out
}

// Every document is valid against the schema, uses nothing deprecated,
// carries no string literal (caller values travel as variables; the only
// literals are the page sizes of nested connections), and its kind
// matches its mutation flag.
func TestDocumentsMatchSchema(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, op := range allOperations() {
		if names[op.name] {
			t.Errorf("operation name %s is used twice", op.name)
		}
		names[op.name] = true
		if p := linearschema.DocumentProblems(op.text, op.name); len(p) > 0 {
			t.Errorf("%s: %s", op.name, strings.Join(p, "; "))
			continue
		}
		doc, _ := parser.ParseQuery(&ast.Source{Input: op.text})
		if got := doc.Operations[0].Operation == ast.Mutation; got != op.mutation {
			t.Errorf("%s: mutation flag %t, document is a %s", op.name, op.mutation, doc.Operations[0].Operation)
		}
		if strings.Contains(op.text, `"`) {
			t.Errorf("%s: the document holds a string literal; pass values as variables", op.name)
		}
	}
}

// Every document a family can send, for each n from 1 to its max, is
// valid against the schema, a mutation with no string literal, and
// declares exactly the variables its aliases use.
func TestFamilyDocumentsMatchSchema(t *testing.T) {
	t.Parallel()
	families := registry.families[:registry.nFam]
	if len(families) == 0 {
		t.Fatal("no families registered")
	}
	for _, f := range families {
		name := f.name
		for i := range f.docs {
			op := f.op(i + 1)
			if p := linearschema.DocumentProblems(op.text, op.name); len(p) > 0 {
				t.Errorf("%s for %d: %s", name, i+1, strings.Join(p, "; "))
				continue
			}
			doc, _ := parser.ParseQuery(&ast.Source{Input: op.text})
			if o := doc.Operations[0]; o.Operation != ast.Mutation || !op.mutation || len(o.SelectionSet) != i+1 {
				t.Errorf("%s for %d: a %s of %d fields", name, i+1, o.Operation, len(o.SelectionSet))
			}
			if strings.Contains(op.text, `"`) {
				t.Errorf("%s for %d: the document holds a string literal", name, i+1)
			}
		}
	}
}

// The schema check refuses what it is there to refuse.
func TestDocumentProblemsRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, doc, want string }{
		{"Q", `query Q { viewer { nope } }`, `Cannot query field "nope"`},
		{"Q", `query Q { team(id: "x") { private } }`, "uses deprecated Team.private"},
		{"Q", `mutation Q($input: CycleCreateInput!) { cycleCreate(input: $input) { success } }`, "uses deprecated Mutation.cycleCreate"},
		{"Q", `query Q($o: PaginationOrderBy) { issues(orderBy: $o) { nodes { boardOrder } } }`, "uses deprecated Issue.boardOrder"},
		{"Q", `query Q($t: String!) { searchIssues(term: $t) { nodes { boardOrder } } }`, "uses deprecated IssueSearchResult.boardOrder"},
		{"Other", `query Q { viewer { id } }`, `want exactly one operation named "Other"`},
		{"Q", `query Q($id: String!) { issue(id: $id) { id } } query R { viewer { id } }`, "want exactly one operation"},
	} {
		got := strings.Join(linearschema.DocumentProblems(tc.doc, tc.name), "; ")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.doc, got, tc.want)
		}
	}
}

// TestMain checks, after a full run, that every document was sent by a
// call some test made, and that the fakes saw no document outside
// allOperations.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && fullRun() {
		known := map[string]bool{}
		exercised := map[string]bool{}
		for _, name := range lineartest.Exercised() {
			exercised[name] = true
		}
		for _, op := range allOperations() {
			known[op.name] = true
			if !exercised[op.name] {
				fmt.Fprintf(os.Stderr, "FAIL: document %s is not exercised by any call under test\n", op.name)
				code = 1
			}
		}
		for name := range exercised {
			if !known[name] {
				fmt.Fprintf(os.Stderr, "FAIL: document %s was sent but is not in allOperations\n", name)
				code = 1
			}
		}
	}
	os.Exit(code)
}

// fullRun reports a run of every test, not one narrowed by -run, -skip
// or -short.
func fullRun() bool {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return false
		}
	}
	return !testing.Short()
}
