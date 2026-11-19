package linear

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// allOperations is every document the package registered, by name.
func allOperations() []operation {
	var out []operation
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		out = append(out, registered[name])
	}
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
		if p := lineartest.DocumentProblems(op.text, op.name); len(p) > 0 {
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
		got := strings.Join(lineartest.DocumentProblems(tc.doc, tc.name), "; ")
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
