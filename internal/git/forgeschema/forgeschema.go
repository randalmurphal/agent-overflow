// Package forgeschema holds the GitHub and gitlab.com GraphQL schemas and
// checks documents and answers against them, for tests: that every
// document internal/git sends is valid and selects nothing deprecated
// (internal/git's graphql_schema_test.go), and that a fake forge's answer
// to one has the shape the document selects (internal/git's test forge,
// the fake forge's rigs in cmd/ao-mockforge).
//
// Only tests import it: the schemas are embedded, about 2 MB. Refresh them
// with `make forge-schemas` (refresh/), never from a test.
package forgeschema

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/core"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

var (
	//go:embed github.graphql
	githubSDL string
	//go:embed gitlab.graphql
	gitlabSDL string
)

var (
	loadOnce sync.Once
	schemas  map[string]*ast.Schema
	loadErr  error
)

// Schema is forge's schema, github or gitlab.
func Schema(forge string) (*ast.Schema, error) {
	loadOnce.Do(func() {
		schemas = map[string]*ast.Schema{}
		for name, sdl := range map[string]string{"github": githubSDL, "gitlab": gitlabSDL} {
			schema, err := validator.LoadSchema(validator.Prelude, &ast.Source{Name: name + ".graphql", Input: sdl})
			if err != nil {
				loadErr = fmt.Errorf("forgeschema: %s: %w", name, err)
				return
			}
			schemas[name] = schema
		}
	})
	if loadErr != nil {
		return nil, loadErr
	}
	schema, ok := schemas[forge]
	if !ok {
		return nil, fmt.Errorf("forgeschema: no schema for forge %q", forge)
	}
	return schema, nil
}

// Check validates document against forge's schema by the spec's
// validation rules (problems) and names every deprecated field,
// argument, enum value or input field it uses, as Type.field,
// Type.field(arg:) or Enum.VALUE, with its deprecation reason.
func Check(forge, document string) (problems []string, deprecated map[string]string, err error) {
	schema, err := Schema(forge)
	if err != nil {
		return nil, nil, err
	}
	_, problems, deprecated = check(schema, document)
	return problems, deprecated, nil
}

func check(schema *ast.Schema, document string) (*ast.QueryDocument, []string, map[string]string) {
	doc, err := parser.ParseQuery(&ast.Source{Input: document})
	if err != nil {
		return nil, []string{err.Error()}, nil
	}
	deprecated := map[string]string{}
	set := rules.NewDefaultRules()
	set.AddRule("NoDeprecated", func(observers *core.Events, _ core.AddErrFunc) {
		use := func(element string, directives ast.DirectiveList) {
			if dir := directives.ForName("deprecated"); dir != nil {
				reason := ""
				if arg := dir.Arguments.ForName("reason"); arg != nil && arg.Value != nil {
					reason = arg.Value.Raw
				}
				deprecated[element] = reason
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
	})
	var problems []string
	for _, err := range validator.ValidateWithRules(schema, doc, set) {
		problems = append(problems, err.Message)
	}
	return doc, problems, deprecated
}

// AnswerProblems checks a GraphQL answer body against the document it
// answers: every member of data is a response name the document selects
// on that object's type (read from its __typename where the field's type
// is abstract), and every built-in scalar and enum value has its JSON
// kind. A member may be absent (a part @include left out, a field a
// fake does not answer); null is not checked, nor a body that is not
// JSON (a cut-short answer is the transport's case). An error means
// document itself is not valid against forge's schema.
func AnswerProblems(forge, document string, body []byte) ([]string, error) {
	schema, err := Schema(forge)
	if err != nil {
		return nil, err
	}
	doc, err := validated(forge, schema, document)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, nil
	}
	var answer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return []string{fmt.Sprintf("the answer is not a GraphQL envelope: %v", err)}, nil
	}
	if len(answer.Data) == 0 || string(answer.Data) == "null" {
		return nil, nil
	}
	var data any
	if err := json.Unmarshal(answer.Data, &data); err != nil {
		return []string{fmt.Sprintf("data: %v", err)}, nil
	}
	op := doc.Operations[0]
	root := schema.Query
	switch op.Operation {
	case ast.Mutation:
		root = schema.Mutation
	case ast.Subscription:
		root = schema.Subscription
	}
	a := answerCheck{schema: schema, fragments: doc.Fragments}
	a.object(op.SelectionSet, root, data, "data")
	return a.problems, nil
}

// ErrInvalidDocument is AnswerProblems' error for a document that is not
// valid against the forge's schema.
var ErrInvalidDocument = errors.New("the document is not valid against the forge's schema")

// validatedDocs caches validated documents by forge and text: validation
// annotates the parsed document with its schema definitions, and a
// document is answered many times.
var validatedDocs sync.Map

type validatedDoc struct {
	doc *ast.QueryDocument
	err error
}

func validated(forge string, schema *ast.Schema, document string) (*ast.QueryDocument, error) {
	key := forge + "\x00" + document
	if v, ok := validatedDocs.Load(key); ok {
		return v.(validatedDoc).doc, v.(validatedDoc).err
	}
	doc, problems, _ := check(schema, document)
	v := validatedDoc{doc: doc}
	if len(problems) > 0 {
		v = validatedDoc{err: fmt.Errorf("%w (%s): %s", ErrInvalidDocument, forge, strings.Join(problems, "; "))}
	}
	actual, _ := validatedDocs.LoadOrStore(key, v)
	return actual.(validatedDoc).doc, actual.(validatedDoc).err
}

type answerCheck struct {
	schema    *ast.Schema
	fragments ast.FragmentDefinitionList
	problems  []string
}

func (a *answerCheck) fail(format string, args ...any) {
	a.problems = append(a.problems, fmt.Sprintf(format, args...))
}

// object checks value, an object of type parent answering selections.
func (a *answerCheck) object(selections ast.SelectionSet, parent *ast.Definition, value any, path string) {
	obj, ok := value.(map[string]any)
	if !ok {
		a.fail("%s: an object of %s is a %T", path, parent.Name, value)
		return
	}
	concrete := parent
	if parent.IsAbstractType() {
		concrete = nil
		if name, ok := obj["__typename"].(string); ok {
			concrete = a.schema.Types[name]
			if concrete == nil || !slices.Contains(a.schema.GetPossibleTypes(parent), concrete) {
				a.fail("%s: __typename %q is no %s", path, name, parent.Name)
				return
			}
		}
	}
	fields := map[string][]*ast.Field{}
	a.collect(selections, concrete, fields)
	for _, key := range sortedKeys(obj) {
		selected := fields[key]
		if len(selected) == 0 {
			a.fail("%s.%s: not selected on %s", path, key, typeName(concrete, parent))
			continue
		}
		var sub ast.SelectionSet
		for _, f := range selected {
			sub = append(sub, f.SelectionSet...)
		}
		a.value(sub, selected[0].Definition.Type, obj[key], path+"."+key)
	}
}

// value checks value against typ.
func (a *answerCheck) value(selections ast.SelectionSet, typ *ast.Type, value any, path string) {
	if value == nil {
		return
	}
	if typ.Elem != nil {
		list, ok := value.([]any)
		if !ok {
			a.fail("%s: a list is a %T", path, value)
			return
		}
		for i, item := range list {
			a.value(selections, typ.Elem, item, path+"["+strconv.Itoa(i)+"]")
		}
		return
	}
	def := a.schema.Types[typ.NamedType]
	switch {
	case def == nil:
		a.fail("%s: unknown type %s", path, typ.NamedType)
	case def.Kind == ast.Object || def.Kind == ast.Interface || def.Kind == ast.Union:
		a.object(selections, def, value, path)
	case def.Kind == ast.Enum:
		if s, ok := value.(string); !ok || def.EnumValues.ForName(s) == nil {
			a.fail("%s: %v is no %s value", path, value, def.Name)
		}
	default:
		want := map[string]string{"Int": "float64", "Float": "float64", "String": "string", "ID": "string", "Boolean": "bool"}[def.Name]
		if got := fmt.Sprintf("%T", value); want != "" && got != want {
			a.fail("%s: a %s is a JSON %s", path, def.Name, got)
		}
	}
}

// collect gathers the fields selections select on concrete, by response
// name; with concrete unknown (an abstract type answered without
// __typename) every fragment counts.
func (a *answerCheck) collect(selections ast.SelectionSet, concrete *ast.Definition, out map[string][]*ast.Field) {
	for _, sel := range selections {
		switch sel := sel.(type) {
		case *ast.Field:
			name := sel.Alias
			if name == "" {
				name = sel.Name
			}
			out[name] = append(out[name], sel)
		case *ast.InlineFragment:
			if a.applies(sel.TypeCondition, concrete) {
				a.collect(sel.SelectionSet, concrete, out)
			}
		case *ast.FragmentSpread:
			if def := a.fragments.ForName(sel.Name); def != nil && a.applies(def.TypeCondition, concrete) {
				a.collect(def.SelectionSet, concrete, out)
			}
		}
	}
}

func (a *answerCheck) applies(condition string, concrete *ast.Definition) bool {
	if condition == "" || concrete == nil || condition == concrete.Name {
		return true
	}
	cond := a.schema.Types[condition]
	return cond != nil && cond.IsAbstractType() && slices.Contains(a.schema.GetPossibleTypes(cond), concrete)
}

// typeName names concrete, or parent when the concrete type is unknown.
func typeName(concrete, parent *ast.Definition) string {
	if concrete != nil {
		return concrete.Name
	}
	return parent.Name
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
