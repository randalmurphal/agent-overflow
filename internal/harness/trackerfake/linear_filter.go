package trackerfake

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
)

// A Linear filter is parsed once, by the pre-pass, into a filterExpr the
// connection then evaluates on each record; the pre-pass refuses any
// part parseFilter cannot read, so whether a filter is handled never
// depends on the records it meets.

// filterExpr is a parsed filter input object: every member holds.
type filterExpr struct {
	members []filterMember
}

// filterMember is and, or, or one field's condition.
type filterMember struct {
	// logic is "and" or "or" over subs; empty for a field.
	logic string
	subs  []*filterExpr
	key   string
	cond  *condition
}

// condition is what one field's value must satisfy: null, and either a
// nested filter on a relation, some of a collection, or comparisons.
type condition struct {
	what string
	// null, when set, is whether the value must be unset.
	null *bool
	// nested is a filter on a relation; an unset relation passes none
	// of it (measured: team {id {eq}} returns no workspace label, while
	// team {null: true} returns them).
	nested *filterExpr
	// some is a filter one item of a collection must pass.
	some *filterExpr
	// kind is the operand type of a comparator; nil for the others.
	kind  *operandKind
	tests []comparison
}

type comparison struct {
	compare func(v, operand any) bool
	operand any
}

// operandKind is one operand type comparators take: how an operand is
// read, how a record's value is read to compare with it, and the
// comparators implemented on it. A comparator the schema defines and
// ops lacks, and an operand read refuses, are unhandled.
type operandKind struct {
	// read is an operand as compared, or a problem naming its form.
	read func(v any) (any, string)
	// value is a record's value as compared, nil when unset; ok is
	// false for a value of another type (the field table and the
	// schema disagree). An unset value fails every comparator but neq
	// and nin (unverified against Linear).
	value func(v any) (any, bool)
	// ops compare a value with an operand; in and nin take a list.
	ops map[string]func(v, operand any) bool
}

// operandKinds are the operand types implemented, by the scalar the
// schema's comparator takes. The Linear client sends id eq and in,
// number in, string eqIgnoreCase and date gte (filter.go and catalog.go
// in internal/tracker/linear).
//
// An ISO 8601 duration for a date ("P2W", "-P2W"), which Linear
// documents as relative to the current date, is unhandled: no client
// sends one, and the documentation leaves open whether a DateTime's
// duration counts from now or from the start of the day.
var operandKinds = map[string]*operandKind{
	"String":                 {read: readString, value: stringValue, ops: equality(true)},
	"ID":                     {read: readString, value: stringValue, ops: equality(false)},
	"Boolean":                {read: readBool, value: boolValue, ops: map[string]func(v, o any) bool{"eq": eq, "neq": neq}},
	"Float":                  {read: readNumber, value: numberValue, ops: ordering()},
	"DateTimeOrDuration":     {read: readDateTime, value: dateTimeValue, ops: ordering()},
	"TimelessDateOrDuration": {read: readDate, value: dateValue, ops: ordering()},
}

// An operand is never nil, so an unset value equals none.
func eq(v, o any) bool  { return v == o }
func neq(v, o any) bool { return v != o }

func in(v, o any) bool {
	list, _ := o.([]any)
	return slices.Contains(list, v)
}

func equality(ignoreCase bool) map[string]func(v, o any) bool {
	ops := map[string]func(v, o any) bool{"eq": eq, "neq": neq, "in": in, "nin": func(v, o any) bool { return !in(v, o) }}
	if ignoreCase {
		ops["eqIgnoreCase"] = func(v, o any) bool {
			s, ok := v.(string)
			return ok && strings.EqualFold(s, o.(string))
		}
	}
	return ops
}

// ordering is equality and order over values read as float64.
func ordering() map[string]func(v, o any) bool {
	ops := equality(false)
	order := func(f func(a, b float64) bool) func(v, o any) bool {
		return func(v, o any) bool {
			a, ok := v.(float64)
			return ok && f(a, o.(float64))
		}
	}
	ops["gt"] = order(func(a, b float64) bool { return a > b })
	ops["gte"] = order(func(a, b float64) bool { return a >= b })
	ops["lt"] = order(func(a, b float64) bool { return a < b })
	ops["lte"] = order(func(a, b float64) bool { return a <= b })
	return ops
}

func readString(v any) (any, string) {
	if s, ok := v.(string); ok {
		return s, ""
	}
	return nil, fmt.Sprintf("%T is not a string", v)
}

func stringValue(v any) (any, bool) {
	switch v.(type) {
	case nil, string:
		return v, true
	}
	return nil, false
}

func readBool(v any) (any, string) {
	if b, ok := v.(bool); ok {
		return b, ""
	}
	return nil, fmt.Sprintf("%T is not a boolean", v)
}

func boolValue(v any) (any, bool) {
	switch v.(type) {
	case nil, bool:
		return v, true
	}
	return nil, false
}

func readNumber(v any) (any, string) {
	switch n := v.(type) {
	case float64:
		return n, ""
	case int64:
		return float64(n), ""
	case int:
		return float64(n), ""
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, ""
		}
	}
	return nil, fmt.Sprintf("%v is not a number", v)
}

func numberValue(v any) (any, bool) {
	switch v.(type) {
	case nil, float64:
		return v, true
	}
	return nil, false
}

// readDateTime reads an absolute DateTime as Unix milliseconds.
func readDateTime(v any) (any, string) {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, fmt.Sprintf("%q is not an RFC 3339 time (a relative duration is not implemented)", s)
	}
	return float64(t.UnixMilli()), ""
}

func dateTimeValue(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case time.Time:
		if t.IsZero() {
			return nil, true
		}
		return float64(t.UnixMilli()), true
	}
	return nil, false
}

// readDate reads a TimelessDate, YYYY-MM-DD, as Unix milliseconds at
// its UTC midnight.
func readDate(v any) (any, string) {
	s, _ := v.(string)
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, fmt.Sprintf("%q is not a YYYY-MM-DD date (a relative duration is not implemented)", s)
	}
	return float64(t.UnixMilli()), ""
}

func dateValue(v any) (any, bool) {
	switch d := v.(type) {
	case nil:
		return nil, true
	case string:
		t, err := time.Parse(time.DateOnly, d)
		if err != nil {
			return nil, false
		}
		return float64(t.UnixMilli()), true
	}
	return nil, false
}

// parseFilter parses filter v, of input type ft, on records of type typ.
// problem names the first part the executor does not implement.
func parseFilter(s *ast.Schema, typ, ft string, v any) (*filterExpr, string) {
	p := &filterParser{s: s}
	x := p.filter(typ, ft, v)
	return x, p.problem
}

type filterParser struct {
	s       *ast.Schema
	problem string
}

func (p *filterParser) fail(format string, args ...any) {
	if p.problem == "" {
		p.problem = fmt.Sprintf(format, args...)
	}
}

func (p *filterParser) filter(typ, ft string, v any) *filterExpr {
	f, _ := v.(map[string]any)
	fields := linearType(typ)
	def := p.s.Types[ft]
	x := &filterExpr{}
	for _, key := range slices.Sorted(maps.Keys(f)) {
		member := f[key]
		switch {
		case member == nil:
		case key == "and" || key == "or":
			list, _ := member.([]any)
			m := filterMember{logic: key}
			for _, sub := range list {
				m.subs = append(m.subs, p.filter(typ, ft, sub))
			}
			x.members = append(x.members, m)
		case fields == nil || !fields.has(key):
			p.fail("filter field %s.%s is not implemented", typ, key)
		default:
			x.members = append(x.members, filterMember{key: key, cond: p.condition(typ+"."+key, def.Fields.ForName(key).Type.Name(), member)})
		}
	}
	return x
}

// condition parses the condition of input type ct on what.
func (p *filterParser) condition(what, ct string, v any) *condition {
	m, _ := v.(map[string]any)
	c := &condition{what: what}
	rest := maps.Clone(m)
	if null, ok := rest["null"]; ok {
		delete(rest, "null")
		b, _ := null.(bool)
		c.null = &b
	}
	def := p.s.Types[ct]
	switch {
	case strings.HasSuffix(ct, "CollectionFilter"):
		some, ok := rest["some"]
		if c.null != nil || !ok || len(rest) != 1 {
			p.fail("filter of %s other than some is not implemented", what)
			return c
		}
		c.some = p.filter(filterNode(ct), def.Fields.ForName("some").Type.Name(), some)
	case strings.HasSuffix(ct, "Filter"):
		if len(rest) > 0 {
			c.nested = p.filter(filterNode(ct), ct, rest)
		}
	default:
		p.comparisons(c, def, rest)
	}
	return c
}

// comparisons parses comparator input type def's members in m, each
// read by the operand kind of def's eq.
func (p *filterParser) comparisons(c *condition, def *ast.Definition, m map[string]any) {
	eqDef := def.Fields.ForName("eq")
	if eqDef != nil {
		c.kind = operandKinds[eqDef.Type.Name()]
	}
	if c.kind == nil {
		p.fail("comparators of %s (%s) are not implemented", c.what, def.Name)
		return
	}
	for _, op := range slices.Sorted(maps.Keys(m)) {
		compare, ok := c.kind.ops[op]
		if !ok {
			p.fail("comparator %s of %s is not implemented", op, c.what)
			continue
		}
		operand, problem := p.operand(c.kind, def.Fields.ForName(op).Type.Elem != nil, m[op])
		if problem != "" {
			p.fail("comparator %s of %s: %s", op, c.what, problem)
			continue
		}
		c.tests = append(c.tests, comparison{compare: compare, operand: operand})
	}
}

func (p *filterParser) operand(k *operandKind, list bool, v any) (any, string) {
	if !list {
		return k.read(v)
	}
	items, _ := v.([]any)
	out := make([]any, len(items))
	for i, item := range items {
		x, problem := k.read(item)
		if problem != "" {
			return nil, problem
		}
		out[i] = x
	}
	return out, ""
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

// holds evaluates x on o. A value the field table gives that the
// condition cannot read is the tables and the schema disagreeing, and
// unhandled.
func (r *gqlRun) holds(x *filterExpr, o gqlObject) bool {
	for _, m := range x.members {
		switch m.logic {
		case "and":
			for _, sub := range m.subs {
				if !r.holds(sub, o) {
					return false
				}
			}
			continue
		case "or":
			if !slices.ContainsFunc(m.subs, func(sub *filterExpr) bool { return r.holds(sub, o) }) {
				return false
			}
			continue
		}
		v, ok := o.value(m.key)
		if !ok {
			r.fail("filter field %s.%s is not implemented", o.typeName(), m.key)
			return false
		}
		if !r.satisfies(m.cond, v) {
			return false
		}
	}
	return true
}

func (r *gqlRun) satisfies(c *condition, v any) bool {
	if c.kind != nil {
		read, ok := c.kind.value(v)
		if !ok {
			r.fail("filter of %s: the field table gives %T, which its comparators do not read", c.what, v)
			return false
		}
		v = read
	}
	if c.null != nil && (v == nil) != *c.null {
		return false
	}
	switch {
	case c.nested != nil:
		obj, ok := v.(gqlObject)
		if !ok && v != nil {
			r.fail("filter of %s: the field table gives %T, not a record", c.what, v)
		}
		return ok && r.holds(c.nested, obj)
	case c.some != nil:
		list, ok := v.([]gqlObject)
		if !ok {
			r.fail("filter of %s: the field table gives %T, not a list of records", c.what, v)
			return false
		}
		return slices.ContainsFunc(list, func(o gqlObject) bool { return r.holds(c.some, o) })
	}
	for _, t := range c.tests {
		if !t.compare(v, t.operand) {
			return false
		}
	}
	return true
}
