package trackerfake

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"

	"agent-overflow/internal/tracker/linear/linearschema"
)

func linearSchema(t *testing.T) *ast.Schema {
	t.Helper()
	s, err := linearschema.Schema()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// emptyLinear is a workspace holding only its viewer.
func emptyLinear() Fixture {
	f := testFixture()
	f.Jira = nil
	f.Linear[0].Teams, f.Linear[0].States, f.Linear[0].Labels = nil, nil, nil
	return f
}

// TestLinearTablesMatchTheSchema: every row of the executor's tables
// names something Linear's schema has, so a row cannot answer for a
// field, argument, member, comparator or sort key that does not exist.
func TestLinearTablesMatchTheSchema(t *testing.T) {
	s := linearSchema(t)
	has := func(typ, name string) bool {
		def := s.Types[typ]
		return def != nil && def.Fields.ForName(name) != nil
	}
	for typ, fields := range linearTypes {
		for _, name := range fields.names() {
			// A row may serve a filter key only, as accessibleTeams does.
			if !has(typ, name) && !has(typ+"Filter", name) && (typ != "IssueSearchResult" || !has("Issue", name)) {
				t.Errorf("%s.%s is in the table but not the schema", typ, name)
			}
		}
	}
	for _, root := range []string{"Query", "Mutation"} {
		for _, name := range linearTypes[root].names() {
			spec, ok := rootSpecOf(root, name)
			if !ok {
				continue
			}
			f := s.Types[root].Fields.ForName(name)
			for _, arg := range spec.args {
				if f.Arguments.ForName(arg) == nil {
					t.Errorf("%s.%s reads argument %s the schema lacks", root, name, arg)
				}
			}
			if spec.payload != "" && !has(f.Type.Name(), string(spec.payload)) {
				t.Errorf("%s.%s answers payload field %s the schema lacks", root, name, spec.payload)
			}
			if spec.input != "" && f.Arguments.ForName("input").Type.Name() != spec.input {
				t.Errorf("%s.%s reads input %s, the schema says %s", root, name, spec.input, f.Arguments.ForName("input").Type.Name())
			}
		}
	}
	for typ, members := range linearInputs {
		for _, m := range members {
			if !has(typ, m) {
				t.Errorf("input member %s.%s is not in the schema", typ, m)
			}
		}
	}
	for _, m := range issueMembers {
		if !has("Issue", m.field) {
			t.Errorf("issue member %s sets Issue.%s, which the schema lacks", m.name, m.field)
		}
	}
	for op := range comparators {
		found := false
		for name, def := range s.Types {
			found = found || strings.HasSuffix(name, "Comparator") && def.Fields.ForName(op) != nil
		}
		if !found {
			t.Errorf("comparator %s is in no comparator of the schema", op)
		}
	}
	for typ, keys := range sortTables {
		for key := range keys {
			if !has(typ+"SortInput", key) {
				t.Errorf("sort key %s.%s is not in the schema", typ, key)
			}
		}
	}
}

// filterProbe is one filter condition and whether the executor
// implements it.
type filterProbe struct {
	doc, rootFilter string
	filter          map[string]any
	handled         bool
}

// filterProbes walks every connection the tables answer that takes a
// filter, and every filter type its members reach, once each; for each
// member of each it makes one probe per comparator or member the schema
// defines, expected handled when the tables implement it.
func filterProbes(s *ast.Schema) []filterProbe {
	type place struct {
		doc        func(inner string) string
		rootFilter string
		path       []string
		ft, node   string
		nested     bool
	}
	var queue []place
	seen := map[string]bool{}
	enqueue := func(p place) {
		if !seen[p.ft] {
			seen[p.ft] = true
			queue = append(queue, p)
		}
	}
	// Connections, by a breadth-first walk of the tables from Query.
	type at struct {
		typ string
		doc func(string) string
	}
	visited := map[string]bool{"Query": true}
	for walk := []at{{"Query", func(s string) string { return s }}}; len(walk) > 0; walk = walk[1:] {
		cur := walk[0]
		for _, name := range linearType(cur.typ).names() {
			def := s.Types[cur.typ].Fields.ForName(name)
			if def == nil || slices.ContainsFunc(def.Arguments, func(a *ast.ArgumentDefinition) bool { return a.Type.NonNull }) {
				continue
			}
			outer := cur.doc
			child := def.Type.Name()
			doc := func(inner string) string { return outer(name + " { " + inner + " }") }
			if isConnection(child) {
				node := s.Types[child].Fields.ForName("nodes").Type.Name()
				if fa := def.Arguments.ForName("filter"); fa != nil {
					enqueue(place{doc: func(inner string) string { return outer(name + "(filter: $f) { nodes { " + inner + " } }") },
						rootFilter: fa.Type.Name(), ft: fa.Type.Name(), node: node})
				}
				child, doc = node, func(inner string) string { return outer(name + " { nodes { " + inner + " } }") }
			}
			if linearTypes[child] != nil && !visited[child] {
				visited[child] = true
				walk = append(walk, at{child, doc})
			}
		}
	}
	var out []filterProbe
	for ; len(queue) > 0; queue = queue[1:] {
		p := queue[0]
		fields := linearType(p.node)
		probe := func(key string, cond any, handled bool) {
			v := map[string]any{key: cond}
			for i := len(p.path) - 1; i >= 0; i-- {
				v = map[string]any{p.path[i]: v}
			}
			out = append(out, filterProbe{doc: p.doc("__typename"), rootFilter: p.rootFilter, filter: v, handled: handled})
		}
		for _, m := range s.Types[p.ft].Fields {
			key, mt := m.Name, m.Type.Name()
			switch {
			case key == "and" || key == "or":
			case key == "null" && p.nested:
				probe(key, true, true)
			case fields == nil || !fields.has(key):
				probe(key, sample(s, m.Type), false)
			case strings.HasSuffix(mt, "CollectionFilter"):
				for _, op := range s.Types[mt].Fields {
					if op.Name == "some" {
						enqueue(place{doc: p.doc, rootFilter: p.rootFilter, path: append(slices.Clone(p.path), key, "some"),
							ft: op.Type.Name(), node: filterNode(mt), nested: true})
						continue
					}
					probe(key, map[string]any{op.Name: sample(s, op.Type)}, op.Name == "null")
				}
			case strings.HasSuffix(mt, "Filter"):
				enqueue(place{doc: p.doc, rootFilter: p.rootFilter, path: append(slices.Clone(p.path), key), ft: mt, node: filterNode(mt), nested: true})
			default:
				for _, op := range s.Types[mt].Fields {
					_, ok := comparators[op.Name]
					probe(key, map[string]any{op.Name: sample(s, op.Type)}, ok || op.Name == "null")
				}
			}
		}
	}
	return out
}

// sample is a value of type t the schema admits.
func sample(s *ast.Schema, t *ast.Type) any {
	if t.Elem != nil {
		return []any{sample(s, t.Elem)}
	}
	def := s.Types[t.NamedType]
	switch def.Kind {
	case ast.Enum:
		return def.EnumValues[0].Name
	case ast.InputObject:
		out := map[string]any{}
		for _, f := range def.Fields {
			if f.Type.NonNull && f.DefaultValue == nil {
				out[f.Name] = sample(s, f.Type)
			}
		}
		return out
	}
	switch def.Name {
	case "Int", "Float":
		return 1
	case "Boolean":
		return true
	case "DateTime", "DateTimeOrDuration":
		return "2026-01-01T00:00:00.000Z"
	case "TimelessDate", "TimelessDateOrDuration":
		return "2026-01-01"
	case "JSON", "JSONObject":
		return map[string]any{}
	}
	return "x"
}

// TestEveryFilterComparator sends every comparator and member the schema
// defines for every filter the executor reads to a workspace with nothing
// to filter: one the tables implement is answered, any other is
// unhandled, whatever the data.
func TestEveryFilterComparator(t *testing.T) {
	s := linearSchema(t)
	g := newRig(t, emptyLinear())
	probes := filterProbes(s)
	if len(probes) < 100 {
		t.Fatalf("only %d probes", len(probes))
	}
	handled := 0
	for _, p := range probes {
		if p.handled {
			handled++
		}
		body := linearBody("P", "query P($f: "+p.rootFilter+") { "+p.doc+" }", map[string]any{"f": p.filter})
		rec := g.do(call{origin: linearOrigin, method: "POST", path: "/graphql", body: body})
		if got := rec.Code == 200; got != p.handled {
			f, _ := json.Marshal(p.filter)
			t.Errorf("%s with filter %s: answered %d, want handled %t: %.300s", p.doc, f, rec.Code, p.handled, rec.Body)
		}
	}
	t.Logf("%d probes, %d of them implemented", len(probes), handled)
}
