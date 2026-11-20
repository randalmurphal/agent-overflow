// Package linearschema owns Linear's public GraphQL schema
// (schema.graphql, without descriptions, refreshed by `make
// linear-schema`) and the checks of a request against it: a document's
// validity and deprecated uses (DocumentProblems) and its variables
// (VariableProblems). The linear client's tests, lineartest's server and
// the harness's fake Linear use it.
//
// It imports neither testing, so non-test code can link it, nor package
// linear, so the client's in-package tests can.
package linearschema

import (
	_ "embed"
	"fmt"
	"slices"
	"sync"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/core"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

// Linear's public schema, without descriptions; `make linear-schema`
// refreshes it.
//
//go:embed schema.graphql
var schemaSDL string

var (
	schemaOnce sync.Once
	schema     *ast.Schema
	schemaErr  error
)

// Schema is Linear's public schema, parsed once.
func Schema() (*ast.Schema, error) {
	schemaOnce.Do(func() {
		schema, schemaErr = validator.LoadSchema(validator.Prelude, &ast.Source{Name: "schema.graphql", Input: schemaSDL})
	})
	return schema, schemaErr
}

// DocumentProblems validates a document against the schema: the spec's
// rules, exactly one operation named name, and nothing deprecated. It
// returns no problems for a valid document.
func DocumentProblems(document, name string) []string {
	s, err := Schema()
	if err != nil {
		return []string{err.Error()}
	}
	doc, perr := parser.ParseQuery(&ast.Source{Input: document})
	if perr != nil {
		return []string{perr.Error()}
	}
	var problems []string
	if len(doc.Operations) != 1 || doc.Operations[0].Name != name {
		problems = append(problems, fmt.Sprintf("want exactly one operation named %q", name))
	}
	deprecated := map[string]string{}
	set := rules.NewDefaultRules()
	set.AddRule("NoDeprecated", noDeprecated(deprecated))
	for _, e := range validator.ValidateWithRules(s, doc, set) {
		problems = append(problems, e.Message)
	}
	for use, reason := range deprecated {
		problems = append(problems, fmt.Sprintf("uses deprecated %s (%s)", use, reason))
	}
	slices.Sort(problems)
	return problems
}

// noDeprecated records every deprecated field, argument, enum value or
// input field a document uses.
func noDeprecated(found map[string]string) func(*core.Events, core.AddErrFunc) {
	return func(observers *core.Events, _ core.AddErrFunc) {
		use := func(element string, directives ast.DirectiveList) {
			if dir := directives.ForName("deprecated"); dir != nil {
				reason := ""
				if arg := dir.Arguments.ForName("reason"); arg != nil && arg.Value != nil {
					reason = arg.Value.Raw
				}
				found[element] = reason
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
	}
}
