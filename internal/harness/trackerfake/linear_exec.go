package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
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
	errors    []gqlErr
	unhandled string
	cost      int
	now       time.Time
}

// linearMaxPage is the largest page (measured).
const linearMaxPage = 250

// execute runs a request already checked against the schema: the
// pre-pass (checkOperation) refuses anything the executor does not
// implement before anything runs. Callers hold mu.
func (e *Engine) execute(w *linearWorkspace, in linearRequest) answer {
	s, err := linearschema.Schema()
	if err != nil {
		return unhandledf("load Linear's schema: %v", err)
	}
	doc, perr := parser.ParseQuery(&ast.Source{Input: in.Query})
	if perr != nil {
		return unhandledf("parse the document: %v", perr)
	}
	if errs := validator.ValidateWithRules(s, doc, nil); len(errs) > 0 {
		return unhandledf("validate the document: %v", errs)
	}
	op := doc.Operations.ForName(in.OperationName)
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
	if problem := checkOperation(s, op, vars); problem != "" {
		return unhandledf("%s", problem)
	}
	r := &gqlRun{e: e, w: w, s: s, vars: vars, now: e.now()}
	w.clock = r.now
	var root gqlObject = queryRoot{w}
	if op.Operation == ast.Mutation {
		root = mutationRoot{w}
	}
	data := r.selectFields(root, op.SelectionSet, nil)
	if r.unhandled != "" {
		return unhandledf("%s", r.unhandled)
	}
	body := map[string]any{"data": data}
	if len(r.errors) > 0 {
		list := make([]any, len(r.errors))
		for i, g := range r.errors {
			list[i] = g.wire()
		}
		body["errors"] = list
		if r.nullsData(op.SelectionSet) {
			body["data"] = nil
		}
	}
	a := jsonAnswer(http.StatusOK, body)
	for k, v := range w.spend(r.now, r.cost+1) {
		a.header[k] = v
	}
	return a
}

// nullsData reports an error on a non-null root field, which makes the
// whole data null.
func (r *gqlRun) nullsData(set ast.SelectionSet) bool {
	for _, g := range r.errors {
		for _, sel := range set {
			if f, ok := sel.(*ast.Field); ok && len(g.path) == 1 && g.path[0] == responseKey(f) && f.Definition.Type.NonNull {
				return true
			}
		}
	}
	return false
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

// selectFields answers set on obj.
func (r *gqlRun) selectFields(obj gqlObject, set ast.SelectionSet, path []any) map[string]any {
	out := map[string]any{}
	r.cost++
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			key := responseKey(s)
			out[key] = r.field(obj, s, append(slices.Clone(path), key))
		case *ast.FragmentSpread:
			r.spread(obj, s.Definition.TypeCondition, s.Definition.SelectionSet, path, out)
		case *ast.InlineFragment:
			r.spread(obj, s.TypeCondition, s.SelectionSet, path, out)
		}
	}
	return out
}

// spread answers a fragment's selection on obj into out. The pre-pass
// refuses a fragment on a type other than its parent's.
func (r *gqlRun) spread(obj gqlObject, cond string, set ast.SelectionSet, path []any, out map[string]any) {
	if cond != "" && cond != obj.typeName() {
		r.fail("a fragment on %s inside %s is not implemented", cond, obj.typeName())
		return
	}
	for k, v := range r.selectFields(obj, set, path) {
		out[k] = v
	}
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

// field answers one field of obj.
func (r *gqlRun) field(obj gqlObject, f *ast.Field, path []any) any {
	if f.Name == "__typename" {
		return obj.typeName()
	}
	args := fieldArgs(f, r.vars)
	var v any
	handled := false
	if af, ok := obj.(argFielder); ok {
		var gerr *gqlErr
		if v, gerr, handled = af.argField(r, f, args); gerr != nil {
			gerr.path = path
			r.errors = append(r.errors, *gerr)
			return nil
		}
	}
	if !handled {
		var ok bool
		if v, ok = obj.value(f.Name); !ok {
			r.fail("field %s.%s is not implemented", obj.typeName(), f.Name)
			return nil
		}
	}
	if typ := f.Definition.Type.Name(); isConnection(typ) {
		list, ok := v.([]gqlObject)
		if !ok {
			r.fail("field %s.%s is not a list the fake pages", obj.typeName(), f.Name)
			return nil
		}
		conn, gerr := r.connection(typ, obj, f.Name, list, args)
		if gerr != nil {
			gerr.path = path
			r.errors = append(r.errors, *gerr)
			return nil
		}
		v = conn
	}
	for k := range args {
		r.fail("argument %s of %s.%s is not implemented", k, obj.typeName(), f.Name)
	}
	return r.render(v, f, path)
}

// render writes a value under f's selection.
func (r *gqlRun) render(v any, f *ast.Field, path []any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case gqlObject:
		return r.selectFields(v, f.SelectionSet, path)
	case []gqlObject:
		out := make([]any, len(v))
		for i, o := range v {
			out[i] = r.selectFields(o, f.SelectionSet, append(slices.Clone(path), i))
		}
		return out
	case time.Time:
		if v.IsZero() {
			return nil
		}
		return linearTime(v)
	case []string:
		return orEmpty(v)
	}
	return v
}

// linearTime is how Linear writes a DateTime.
func linearTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// take consumes an argument from a field's own argument map.
func take(args map[string]any, name string) (any, bool) {
	v, ok := args[name]
	delete(args, name)
	return v, ok
}
