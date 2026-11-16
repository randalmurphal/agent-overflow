package linear

import (
	_ "embed"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/core"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

// Linear's public schema, without descriptions; `make linear-schema`
// refreshes it.
//
//go:embed testdata/schema.graphql
var schemaSDL string

// allOperations is every document the package registered, by name.
func allOperations() []operation {
	var out []operation
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		out = append(out, registered[name])
	}
	return out
}

// exercised records the operations a fake answered during this run.
var exercised sync.Map

var (
	schemaOnce sync.Once
	schema     *ast.Schema
	schemaErr  error
)

func loadSchema() (*ast.Schema, error) {
	schemaOnce.Do(func() {
		schema, schemaErr = validator.LoadSchema(validator.Prelude, &ast.Source{Name: "schema.graphql", Input: schemaSDL})
	})
	return schema, schemaErr
}

// documentProblems validates a document against the schema: the spec's
// rules, one operation named name, and nothing deprecated.
func documentProblems(document, name string) []string {
	s, err := loadSchema()
	if err != nil {
		return []string{err.Error()}
	}
	doc, perr := parser.ParseQuery(&ast.Source{Input: document})
	if perr != nil {
		return []string{perr.Error()}
	}
	var problems []string
	if len(doc.Operations) != 1 || doc.Operations[0].Name != name {
		problems = append(problems, fmt.Sprintf("want exactly one operation named %q", name))
	}
	deprecated := map[string]string{}
	set := rules.NewDefaultRules()
	set.AddRule("NoDeprecated", noDeprecated(deprecated))
	for _, e := range validator.ValidateWithRules(s, doc, set) {
		problems = append(problems, e.Message)
	}
	for use, reason := range deprecated {
		problems = append(problems, fmt.Sprintf("uses deprecated %s (%s)", use, reason))
	}
	slices.Sort(problems)
	return problems
}

// noDeprecated records every deprecated field, argument, enum value or
// input field a document uses.
func noDeprecated(found map[string]string) func(*core.Events, core.AddErrFunc) {
	return func(observers *core.Events, _ core.AddErrFunc) {
		use := func(element string, directives ast.DirectiveList) {
			if dir := directives.ForName("deprecated"); dir != nil {
				reason := ""
				if arg := dir.Arguments.ForName("reason"); arg != nil && arg.Value != nil {
					reason = arg.Value.Raw
				}
				found[element] = reason
			}
		}
		observers.OnField(func(_ *core.Walker, field *ast.Field) {
			if field.Definition == nil || field.ObjectDefinition == nil {
				return
			}
			owner := field.ObjectDefinition.Name + "." + field.Name
			use(owner, field.Definition.Directives)
			for _, arg := range field.Arguments {
				if def := field.Definition.Arguments.ForName(arg.Name); def != nil {
					use(owner+"("+arg.Name+":)", def.Directives)
				}
			}
		})
		observers.OnValue(func(_ *core.Walker, value *ast.Value) {
			if value.Definition == nil {
				return
			}
			switch value.Kind {
			case ast.EnumValue:
				if def := value.Definition.EnumValues.ForName(value.Raw); def != nil {
					use(value.Definition.Name+"."+value.Raw, def.Directives)
				}
			case ast.ObjectValue:
				for _, child := range value.Children {
					if def := value.Definition.Fields.ForName(child.Name); def != nil {
						use(value.Definition.Name+"."+child.Name, def.Directives)
					}
				}
			}
		})
	}
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
		if p := documentProblems(op.text, op.name); len(p) > 0 {
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
		{"Other", `query Q { viewer { id } }`, `want exactly one operation named "Other"`},
		{"Q", `query Q($id: String!) { issue(id: $id) { id } } query R { viewer { id } }`, "want exactly one operation"},
	} {
		got := strings.Join(documentProblems(tc.doc, tc.name), "; ")
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
		for _, op := range allOperations() {
			known[op.name] = true
			if _, ok := exercised.Load(op.name); !ok {
				fmt.Fprintf(os.Stderr, "FAIL: document %s is not exercised by any call under test\n", op.name)
				code = 1
			}
		}
		exercised.Range(func(name, _ any) bool {
			if !known[name.(string)] {
				fmt.Fprintf(os.Stderr, "FAIL: document %s was sent but is not in allOperations\n", name)
				code = 1
			}
			return true
		})
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
