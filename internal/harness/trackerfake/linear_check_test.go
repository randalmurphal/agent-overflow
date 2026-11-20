package trackerfake

import (
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
	for scalar, kind := range operandKinds {
		for op := range kind.ops {
			found := false
			for name, def := range s.Types {
				eq, f := def.Fields.ForName("eq"), def.Fields.ForName(op)
				found = found || strings.HasSuffix(name, "Comparator") && eq != nil && eq.Type.Name() == scalar && f != nil && f.Type.Name() == scalar
			}
			if !found {
				t.Errorf("comparator %s on %s is in no comparator of the schema", op, scalar)
			}
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
// implements it. A comparator's probe also names its operand's scalar
// and comparator, and at builds the filter for another condition at
// the same place.
type filterProbe struct {
	doc, rootFilter string
	filter          map[string]any
	handled         bool
	scalar, op      string
	list            bool
	at              func(cond any) map[string]any
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
		at := func(key string) func(cond any) map[string]any {
			path := slices.Clone(p.path)
			return func(cond any) map[string]any {
				v := map[string]any{key: cond}
				for i := len(path) - 1; i >= 0; i-- {
					v = map[string]any{path[i]: v}
				}
				return v
			}
		}
		probe := func(key string, cond any, handled bool) *filterProbe {
			out = append(out, filterProbe{doc: p.doc("__typename"), rootFilter: p.rootFilter, filter: at(key)(cond), handled: handled, at: at(key)})
			return &out[len(out)-1]
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
					// Only some is implemented; a collection's null is not
					// (what Linear means by it is unverified).
					probe(key, map[string]any{op.Name: sample(s, op.Type)}, false)
				}
			case strings.HasSuffix(mt, "Filter"):
				enqueue(place{doc: p.doc, rootFilter: p.rootFilter, path: append(slices.Clone(p.path), key), ft: mt, node: filterNode(mt), nested: true})
			default:
				scalar := ""
				if eq := s.Types[mt].Fields.ForName("eq"); eq != nil {
					scalar = eq.Type.Name()
				}
				kind := operandKinds[scalar]
				for _, op := range s.Types[mt].Fields {
					handled := kind != nil && (op.Name == "null" || kind.ops[op.Name] != nil)
					pr := probe(key, map[string]any{op.Name: sample(s, op.Type)}, handled)
					pr.scalar, pr.op, pr.list = scalar, op.Name, op.Type.Elem != nil
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
