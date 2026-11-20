package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"

	"agent-overflow/internal/tracker/linear/linearschema"
)

// gqlObject is an object the executor reads fields of. value is a
// field's value: nil, a string, float64, bool or time.Time scalar, a
// []string, an object, or a list of objects for a list or connection;
// ok is false for a field the fake does not implement.
type gqlObject interface {
	typeName() string
	value(name string) (any, bool)
}

// argFielder is an object some of whose fields take arguments of their
// own (the query and mutation roots); handled is false for any other
// field. It consumes the arguments it reads from args.
type argFielder interface {
	argField(r *gqlRun, name string, args map[string]any) (v any, err *gqlErr, handled bool)
}

// gqlRun is one request's execution.
type gqlRun struct {
	e         *Engine
	w         *linearWorkspace
	doc       *ast.QueryDocument
	vars      map[string]any
	errors    []gqlErr
	unhandled string
	cost      int
	now       time.Time
}

// linearMaxPage is the largest page (measured).
const linearMaxPage = 250

// execute runs a request already checked against the schema. Callers
// hold mu.
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
	r := &gqlRun{e: e, w: w, doc: doc, vars: vars, now: e.now()}
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
			if s.Definition.TypeCondition == obj.typeName() {
				for k, v := range r.selectFields(obj, s.Definition.SelectionSet, path) {
					out[k] = v
				}
			} else {
				r.fail("fragment %s on %s spread on %s", s.Name, s.Definition.TypeCondition, obj.typeName())
			}
		case *ast.InlineFragment:
			if s.TypeCondition == "" || s.TypeCondition == obj.typeName() {
				for k, v := range r.selectFields(obj, s.SelectionSet, path) {
					out[k] = v
				}
			}
		}
	}
	return out
}

// field answers one field of obj.
func (r *gqlRun) field(obj gqlObject, f *ast.Field, path []any) any {
	if f.Name == "__typename" {
		return obj.typeName()
	}
	args := f.ArgumentMap(r.vars)
	for k, v := range args {
		if v == nil {
			delete(args, k)
		}
	}
	var v any
	handled := false
	if af, ok := obj.(argFielder); ok {
		var gerr *gqlErr
		if v, gerr, handled = af.argField(r, f.Name, args); gerr != nil {
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
	if typ := f.Definition.Type.Name(); strings.HasSuffix(typ, "Connection") || strings.HasSuffix(typ, "SearchPayload") {
		list, ok := v.([]gqlObject)
		if !ok {
			r.fail("field %s.%s is not a list the fake pages", obj.typeName(), f.Name)
			return nil
		}
		conn, gerr := r.connection(obj, f.Name, list, args)
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

// take consumes an argument.
func take(args map[string]any, name string) (any, bool) {
	v, ok := args[name]
	delete(args, name)
	return v, ok
}
