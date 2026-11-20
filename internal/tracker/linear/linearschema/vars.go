package linearschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// VariableProblems checks a request's variables against the variable
// definitions of its operation in the schema: no undeclared variable,
// every required one present and non-null, and each value of its
// declared type, input object fields and enum values included. Enum
// values are compared with their exact case, which gqlparser's own
// coercion does not do.
func VariableProblems(document, name string, raw json.RawMessage) []string {
	s, err := Schema()
	if err != nil {
		return []string{err.Error()}
	}
	doc, perr := parser.ParseQuery(&ast.Source{Input: document})
	if perr != nil {
		return []string{perr.Error()}
	}
	op := doc.Operations.ForName(name)
	if op == nil {
		return []string{fmt.Sprintf("no operation %s", name)}
	}
	vars := map[string]any{}
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&vars); err != nil {
			return []string{"variables are not an object: " + err.Error()}
		}
	}
	v := varCheck{schema: s}
	for key := range vars {
		if op.VariableDefinitions.ForName(key) == nil {
			v.fail("$%s is not declared", key)
		}
	}
	for _, def := range op.VariableDefinitions {
		value, present := vars[def.Variable]
		if !present {
			if def.Type.NonNull && def.DefaultValue == nil {
				v.fail("required $%s is missing", def.Variable)
			}
			continue
		}
		v.value("$"+def.Variable, def.Type, value)
	}
	slices.Sort(v.problems)
	return v.problems
}

type varCheck struct {
	schema   *ast.Schema
	problems []string
}

func (v *varCheck) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *varCheck) value(path string, t *ast.Type, value any) {
	if value == nil {
		if t.NonNull {
			v.fail("%s is null but %s is required", path, t)
		}
		return
	}
	if t.Elem != nil {
		list, ok := value.([]any)
		if !ok {
			v.fail("%s is not a list (%s)", path, t)
			return
		}
		for i, item := range list {
			v.value(fmt.Sprintf("%s[%d]", path, i), t.Elem, item)
		}
		return
	}
	def := v.schema.Types[t.NamedType]
	if def == nil {
		v.fail("%s has unknown type %s", path, t.NamedType)
		return
	}
	switch def.Kind {
	case ast.Scalar:
		v.scalar(path, def.Name, value)
	case ast.Enum:
		s, ok := value.(string)
		if !ok || !slices.ContainsFunc(def.EnumValues, func(e *ast.EnumValueDefinition) bool { return e.Name == s }) {
			v.fail("%s: %v is not a value of %s", path, value, def.Name)
		}
	case ast.InputObject:
		v.object(path, def, value)
	default:
		v.fail("%s has non-input type %s", path, def.Name)
	}
}

func (v *varCheck) object(path string, def *ast.Definition, value any) {
	fields, ok := value.(map[string]any)
	if !ok {
		v.fail("%s is not an object (%s)", path, def.Name)
		return
	}
	for key, item := range fields {
		f := def.Fields.ForName(key)
		if f == nil {
			v.fail("%s.%s is not a field of %s", path, key, def.Name)
			continue
		}
		v.value(path+"."+key, f.Type, item)
	}
	for _, f := range def.Fields {
		if _, present := fields[f.Name]; !present && f.Type.NonNull && f.DefaultValue == nil {
			v.fail("%s.%s is required by %s", path, f.Name, def.Name)
		}
	}
}

func (v *varCheck) scalar(path, name string, value any) {
	ok := true
	switch name {
	case "Int":
		n, isNum := value.(json.Number)
		i, err := n.Int64()
		ok = isNum && err == nil && i >= math.MinInt32 && i <= math.MaxInt32
	case "Float":
		n, isNum := value.(json.Number)
		_, err := n.Float64()
		ok = isNum && err == nil
	case "Boolean":
		_, ok = value.(bool)
	case "JSON", "JSONObject":
	case "TimelessDate":
		s, isText := value.(string)
		_, err := time.Parse(time.DateOnly, s)
		ok = isText && err == nil
	case "DateTime":
		s, isText := value.(string)
		_, err := time.Parse(time.RFC3339Nano, s)
		ok = isText && err == nil
	default: // String, ID and Linear's other string scalars, such as UUID
		_, ok = value.(string)
	}
	if !ok {
		v.fail("%s: %v is not a %s", path, value, name)
	}
}
