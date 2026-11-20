package trackerfake

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// checkOperation is the executor's pre-pass. It walks op by the schema's
// types, as the executor would, and names the first part the executor
// does not implement: a field, argument, directive or fragment, a filter
// key or comparator, a sort, or an input member. It reads the tables the
// resolvers answer from (linear_types.go, rootSpecOf, linearInputs), so
// a request is refused before anything runs, whatever the fixture holds.
//
// No client sends a directive, so every directive is unhandled rather
// than implemented.
func checkOperation(s *ast.Schema, op *ast.OperationDefinition, vars map[string]any) string {
	c := &checker{s: s, vars: vars}
	c.directives("operation "+op.Name, op.Directives)
	for _, v := range op.VariableDefinitions {
		c.directives("variable $"+v.Variable, v.Directives)
	}
	root := "Query"
	switch op.Operation {
	case ast.Mutation:
		root = "Mutation"
	case ast.Subscription:
		c.fail("subscriptions are not implemented")
	}
	c.selections(root, linearType(root), op.SelectionSet)
	return c.problem
}

type checker struct {
	s       *ast.Schema
	vars    map[string]any
	problem string
}

func (c *checker) fail(format string, args ...any) {
	if c.problem == "" {
		c.problem = fmt.Sprintf(format, args...)
	}
}

func (c *checker) directives(where string, list ast.DirectiveList) {
	for _, d := range list {
		c.fail("directive @%s on %s is not implemented", d.Name, where)
	}
}

// selections checks set on typ, answered by fields.
func (c *checker) selections(typ string, fields fieldSet, set ast.SelectionSet) {
	if fields == nil {
		c.fail("type %s is not implemented", typ)
		return
	}
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			c.field(typ, fields, s)
		case *ast.FragmentSpread:
			c.directives("fragment spread "+s.Name, s.Directives)
			c.directives("fragment "+s.Name, s.Definition.Directives)
			c.fragment(typ, fields, s.Definition.TypeCondition, s.Definition.SelectionSet)
		case *ast.InlineFragment:
			c.directives("an inline fragment", s.Directives)
			c.fragment(typ, fields, s.TypeCondition, s.SelectionSet)
		}
	}
}

func (c *checker) fragment(typ string, fields fieldSet, cond string, set ast.SelectionSet) {
	if cond != "" && cond != typ {
		c.fail("a fragment on %s inside %s is not implemented", cond, typ)
		return
	}
	c.selections(typ, fields, set)
}

func (c *checker) field(typ string, fields fieldSet, f *ast.Field) {
	c.directives("field "+typ+"."+f.Name, f.Directives)
	if f.Name == "__typename" {
		return
	}
	if !fields.has(f.Name) {
		c.fail("field %s.%s is not implemented", typ, f.Name)
		return
	}
	child := f.Definition.Type.Name()
	spec, root := rootSpecOf(typ, f.Name)
	declared := spec.args
	if isConnection(child) {
		declared = append(slices.Clone(declared), connectionArgs...)
	}
	args := fieldArgs(f, c.vars)
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if !slices.Contains(declared, k) {
			c.fail("argument %s of %s.%s is not implemented", k, typ, f.Name)
		}
	}
	if isConnection(child) {
		node := c.s.Types[child].Fields.ForName("nodes").Type.Name()
		if v, ok := args["filter"]; ok {
			c.filter(node, f.Definition.Arguments.ForName("filter").Type.Name(), v)
		}
		if v, ok := args["sort"]; ok {
			c.sort(typ+"."+f.Name, node, v)
		}
	}
	if v, ok := args["input"]; ok && spec.input != "" {
		c.input(spec.input, v)
	}
	next := linearType(child)
	if root && typ == "Mutation" {
		next = spec.payload
	}
	if len(f.SelectionSet) > 0 {
		c.selections(child, next, f.SelectionSet)
	}
}

// filter checks a filter of input type ft on typ: each key a field of
// typ's table, each condition one the executor evaluates (compare).
func (c *checker) filter(typ, ft string, v any) {
	f, _ := v.(map[string]any)
	fields := linearType(typ)
	def := c.s.Types[ft]
	for _, key := range slices.Sorted(maps.Keys(f)) {
		cond := f[key]
		switch {
		case cond == nil:
		case key == "and" || key == "or":
			list, _ := cond.([]any)
			for _, sub := range list {
				c.filter(typ, ft, sub)
			}
		case fields == nil || !fields.has(key):
			c.fail("filter field %s.%s is not implemented", typ, key)
		default:
			c.condition(typ+"."+key, def.Fields.ForName(key).Type.Name(), cond)
		}
	}
}

// condition checks a condition of input type ct: null alone, some of a
// collection filter, a nested filter, or scalar comparators.
func (c *checker) condition(what, ct string, v any) {
	cond, _ := v.(map[string]any)
	if _, ok := cond["null"]; ok && len(cond) == 1 {
		return
	}
	switch {
	case strings.HasSuffix(ct, "CollectionFilter"):
		some, ok := cond["some"]
		if !ok || len(cond) != 1 {
			c.fail("filter of %s other than some is not implemented", what)
			return
		}
		c.filter(filterNode(ct), c.s.Types[ct].Fields.ForName("some").Type.Name(), some)
	case strings.HasSuffix(ct, "Filter"):
		c.filter(filterNode(ct), ct, cond)
	default:
		for _, op := range slices.Sorted(maps.Keys(cond)) {
			if _, ok := comparators[op]; !ok {
				c.fail("comparator %s of %s is not implemented", op, what)
			}
		}
	}
}

// filterNode is the object type a filter input type filters, by the
// schema's naming: TeamFilter, NullableTeamFilter and
// TeamCollectionFilter filter Team.
func filterNode(ft string) string {
	ft = strings.TrimPrefix(ft, "Nullable")
	if node, ok := strings.CutSuffix(ft, "CollectionFilter"); ok {
		return node
	}
	return strings.TrimSuffix(ft, "Filter")
}

func (c *checker) sort(what, node string, v any) {
	field, _, problem := sortSpec(v)
	if problem != "" {
		c.fail("%s: %s", what, problem)
		return
	}
	if _, ok := sortTables[node][field]; !ok {
		c.fail("sort of %s by %s is not implemented", node, field)
	}
}

func (c *checker) input(typ string, v any) {
	m, _ := v.(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(linearInputs[typ], k) {
			c.fail("input member %s.%s is not implemented", typ, k)
		}
	}
}
