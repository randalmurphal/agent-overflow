package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator"

	"agent-overflow/internal/tracker/linear/linearschema"
)

// gqlObject is an object the executor reads fields of, through its type's
// field table (linear_types.go); ok is false for a field the fake does
// not implement.
type gqlObject interface {
	typeName() string
	value(name string) (any, bool)
}

// argFielder is a root whose rootFields include fields with arguments of
// their own; handled is false for any other field.
type argFielder interface {
	argField(r *gqlRun, f *ast.Field, args map[string]any) (v any, err *gqlErr, handled bool)
}

// gqlRun is one request's execution.
type gqlRun struct {
	e         *Engine
	w         *linearWorkspace
	s         *ast.Schema
	vars      map[string]any
	filters   map[*ast.Field]*filterExpr
	errors    []gqlErr
	unhandled string
	now       time.Time
}

// linearMaxPage is the largest page (measured).
const linearMaxPage = 250

// execute runs op, the operation of doc that in names, already checked
// against the schema: the pre-pass (checkOperation) refuses anything the
// executor does not implement before anything runs. rollback is the
// pipeline's undo of the request (txn.go). Callers hold mu.
func (e *Engine) execute(w *linearWorkspace, in linearRequest, doc *ast.QueryDocument, op *ast.OperationDefinition, rollback func()) answer {
	s, err := linearschema.Schema()
	if err != nil {
		return unhandledf("load Linear's schema: %v", err)
	}
	if errs := validator.ValidateWithRules(s, doc, nil); len(errs) > 0 {
		return unhandledf("validate the document: %v", errs)
	}
	raw := map[string]any{}
	if len(in.Variables) > 0 && string(in.Variables) != "null" {
		if err := json.Unmarshal(in.Variables, &raw); err != nil {
			return unhandledf("variables: %v", err)
		}
	}
	vars, verr := validator.VariableValues(s, op, raw)
	if verr != nil {
		return unhandledf("variables: %v", verr)
	}
	filters, problem := checkOperation(s, op, vars)
	if problem != "" {
		return unhandledf("%s", problem)
	}
	r := &gqlRun{e: e, w: w, s: s, vars: vars, filters: filters, now: e.now()}
	w.clock = r.now
	var root gqlObject = queryRoot{w}
	if op.Operation == ast.Mutation {
		root = mutationRoot{w}
	}
	data, nulled := r.selectFields(root, op.SelectionSet, nil)
	if r.unhandled != "" {
		return unhandledf("%s", r.unhandled)
	}
	// Measured: three aliased issueUpdate fields with the middle one
	// naming a missing issue answered data null with the error at that
	// alias, and neither other update was applied. So a mutation
	// document with any error changes nothing (one case measured; the
	// fake applies it to every document).
	if op.Operation == ast.Mutation && len(r.errors) > 0 {
		rollback()
	}
	body := map[string]any{"data": data}
	if nulled {
		body["data"] = nil
	}
	if len(r.errors) > 0 {
		list := make([]any, len(r.errors))
		for i, g := range r.errors {
			list[i] = g.wire()
		}
		body["errors"] = list
	}
	a := jsonAnswer(http.StatusOK, body)
	for k, v := range w.spend(r.now, r.linearCost(op)) {
		a.header[k] = v
	}
	// Every HTTP answer carries Date (RFC 9110); the fake's is its own
	// clock, so a client that takes the tracker's time from it follows
	// the fake's clock and not the computer's.
	a.header.Set("Date", r.now.UTC().Format(http.TimeFormat))
	return a
}

func responseKey(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// fail records the first unhandled part of the document.
func (r *gqlRun) fail(format string, args ...any) {
	if r.unhandled == "" {
		r.unhandled = fmt.Sprintf(format, args...)
	}
}

// collected is one response key's fields in a selection set, those of
// the fragments it spreads included: the key is answered once, from its
// first field, under the sub-selections of all of them merged. Two
// fields of one key with another name or other arguments never get
// here: the schema check (gqlparser's OverlappingFieldsCanBeMerged)
// refuses them.
type collected struct {
	key    string
	fields []*ast.Field
}

// collectFields is set's fields on a typ object by response key, in
// document order (GraphQL's CollectFields). The pre-pass refuses a
// fragment on a type other than its parent's.
func (r *gqlRun) collectFields(typ string, set ast.SelectionSet) []collected {
	var out []collected
	index := map[string]int{}
	var walk func(set ast.SelectionSet)
	walk = func(set ast.SelectionSet) {
		for _, sel := range set {
			var cond string
			var inner ast.SelectionSet
			switch s := sel.(type) {
			case *ast.Field:
				key := responseKey(s)
				if i, ok := index[key]; ok {
					out[i].fields = append(out[i].fields, s)
					continue
				}
				index[key] = len(out)
				out = append(out, collected{key: key, fields: []*ast.Field{s}})
				continue
			case *ast.FragmentSpread:
				cond, inner = s.Definition.TypeCondition, s.Definition.SelectionSet
			case *ast.InlineFragment:
				cond, inner = s.TypeCondition, s.SelectionSet
			}
			if cond != "" && cond != typ {
				r.fail("a fragment on %s inside %s is not implemented", cond, typ)
				continue
			}
			walk(inner)
		}
	}
	walk(set)
	return out
}

// selectFields answers set on obj, each response key once. nulled
// reports a non-null field that an error left null, which makes obj
// null in its turn (GraphQL's null propagation); every field is still
// answered, so each mutation of a document runs.
func (r *gqlRun) selectFields(obj gqlObject, set ast.SelectionSet, path []any) (out map[string]any, nulled bool) {
	out = map[string]any{}
	for _, c := range r.collectFields(obj.typeName(), set) {
		f := c.fields[0]
		var sub ast.SelectionSet
		for _, g := range c.fields {
			sub = append(sub, g.SelectionSet...)
		}
		v, failed := r.field(obj, f, sub, append(slices.Clone(path), c.key))
		out[c.key] = v
		nulled = nulled || failed && f.Definition.Type.NonNull
	}
	if nulled {
		return nil, true
	}
	return out, false
}

// fieldArgs are f's arguments with vars applied, those null left out:
// a map of the field's own, whose values may be shared with vars and
// are never changed.
func fieldArgs(f *ast.Field, vars map[string]any) map[string]any {
	args := f.ArgumentMap(vars)
	for k, v := range args {
		if v == nil {
			delete(args, k)
		}
	}
	return args
}

// field answers one field of obj under the selection sub. failed
// reports that an error left it null, at the field or at a non-null
// field under it.
func (r *gqlRun) field(obj gqlObject, f *ast.Field, sub ast.SelectionSet, path []any) (v any, failed bool) {
	if f.Name == "__typename" {
		return obj.typeName(), false
	}
	args := fieldArgs(f, r.vars)
	handled := false
	if af, ok := obj.(argFielder); ok {
		var gerr *gqlErr
		if v, gerr, handled = af.argField(r, f, args); gerr != nil {
			gerr.path = path
			r.errors = append(r.errors, *gerr)
			return nil, true
		}
	}
	if !handled {
		var ok bool
		if v, ok = obj.value(f.Name); !ok {
			r.fail("field %s.%s is not implemented", obj.typeName(), f.Name)
			return nil, false
		}
	}
	if typ := f.Definition.Type.Name(); isConnection(typ) {
		list, ok := v.([]gqlObject)
		if !ok {
			r.fail("field %s.%s is not a list the fake pages", obj.typeName(), f.Name)
			return nil, false
		}
		conn, gerr := r.connection(typ, obj, f.Name, list, args, r.filters[f])
		if gerr != nil {
			gerr.path = path
			r.errors = append(r.errors, *gerr)
			return nil, true
		}
		v = conn
	}
	for k := range args {
		r.fail("argument %s of %s.%s is not implemented", k, obj.typeName(), f.Name)
	}
	return r.render(v, f.Definition.Type, sub, path)
}

// render writes a value of schema type t under the selection sub. failed
// reports a non-null object or list item an error left null.
func (r *gqlRun) render(v any, t *ast.Type, sub ast.SelectionSet, path []any) (any, bool) {
	switch v := v.(type) {
	case nil:
		return nil, false
	case gqlObject:
		return r.selectFields(v, sub, path)
	case []gqlObject:
		out := make([]any, len(v))
		for i, o := range v {
			item, failed := r.selectFields(o, sub, append(slices.Clone(path), i))
			if failed && t.Elem != nil && t.Elem.NonNull {
				return nil, true
			}
			out[i] = item
		}
		return out, false
	case time.Time:
		if v.IsZero() {
			return nil, false
		}
		return linearTime(v), false
	case []string:
		return orEmpty(v), false
	}
	return v, false
}

// linearTime is how Linear writes a DateTime.
func linearTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// take consumes an argument from a field's own argument map.
func take(args map[string]any, name string) (any, bool) {
	v, ok := args[name]
	delete(args, name)
	return v, ok
}
