package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// variableProblems checks a request's variables against the variable
// definitions of its operation in the schema: no undeclared variable,
// every required one present and non-null, and each value of its
// declared type, input object fields and enum values included. Enum
// values are compared with their exact case, which gqlparser's own
// coercion does not do.
func variableProblems(document, name string, raw json.RawMessage) []string {
	s, err := loadSchema()
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
	default: // String, ID and Linear's string scalars: DateTime, TimelessDate, UUID
		_, ok = value.(string)
	}
	if !ok {
		v.fail("%s: %v is not a %s", path, value, name)
	}
}

// The variable check refuses what the schema refuses, independently of
// any expected JSON.
func TestVariableProblems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		op   operation
		vars string
		want string
	}{
		{"valid", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks"}}`, ""},
		{"unknown enum value", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"bogus"}}`, "bogus is not a value of IssueRelationType"},
		{"enum case", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"Blocks"}}`, "Blocks is not a value of IssueRelationType"},
		{"missing required field", opCreateRelation, `{"input":{"issueId":"WEB-1","type":"blocks"}}`, "relatedIssueId is required"},
		{"unknown field", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks","kind":"x"}}`, "kind is not a field"},
		{"missing variable", opCreateRelation, `{}`, "required $input is missing"},
		{"null required", opUpdateIssue, `{"id":null,"input":{}}`, "$id is null"},
		{"undeclared variable", opViewer, `{"x":1}`, "$x is not declared"},
		{"wrong scalar", opTeams, `{"first":"100"}`, "is not a Int"},
		{"fraction for Int", opTeams, `{"first":1.5}`, "is not a Int"},
		{"Int out of range", opTeams, `{"first":3000000000}`, "is not a Int"},
		{"sort enum case", opIssues, `{"first":100,"sort":[{"updatedAt":{"order":"ascending"}}]}`, "ascending is not a value of PaginationSortOrder"},
		{"list expected", opIssues, `{"first":100,"sort":{"updatedAt":{"order":"Ascending"}}}`, "is not a list"},
		{"nullable null", opUpdateIssue, `{"id":"WEB-1","input":{"assigneeId":null,"labelIds":[]}}`, ""},
	} {
		got := strings.Join(variableProblems(tc.op.text, tc.op.name, json.RawMessage(tc.vars)), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The fake refuses a request whose variables the schema refuses, even
// when a test expects exactly those variables.
func TestFakeRefusesInvalidVariables(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var mu sync.Mutex
	var got []string
	f.mu.Lock()
	f.report = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf(format, args...))
	}
	f.mu.Unlock()
	_, err := f.client(t).CreateRelation(t.Context(), CreateRelationInput{IssueID: "WEB-1", RelatedIssueID: "WEB-2", Type: "bogus"})
	if err == nil {
		t.Fatal("the fake answered invalid variables")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "bogus is not a value of IssueRelationType") {
		t.Errorf("reports %q", got)
	}
}
