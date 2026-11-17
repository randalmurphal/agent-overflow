// refresh rewrites the forge GraphQL schemas of package forgeschema:
// GitHub's public schema and gitlab.com's, each written as SDL without
// descriptions, its definitions sorted by name, so a refresh diffs by
// what changed. Deprecation markers stay. It sends no token. Tests never
// run it: refresh by hand on the host network and commit the result.
//
//	make forge-schemas
//	go run ./internal/git/forgeschema/refresh [-github FILE] [-gitlab FILE] [-out DIR]
//
// -github names a saved copy of GitHub's SDL and -gitlab a saved
// introspection answer (the JSON body of introspectionQuery) to convert
// instead of fetching.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
)

const (
	githubSchemaURL = "https://docs.github.com/public/fpt/schema.docs.graphql"
	gitlabGraphQL   = "https://gitlab.com/api/graphql"
	// maxSchemaBytes bounds a download; GitHub's SDL is about 1.6 MB and
	// gitlab.com's introspection answer about 9 MB.
	maxSchemaBytes = 64 << 20
)

// introspectionQuery is the spec's introspection query without
// descriptions, with deprecated arguments and input fields.
const introspectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name } mutationType { name } subscriptionType { name }
    types { ...FullType }
    directives { name locations isRepeatable args(includeDeprecated: true) { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name
  fields(includeDeprecated: true) { name args(includeDeprecated: true) { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason }
  inputFields(includeDeprecated: true) { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name type { ...TypeRef } defaultValue isDeprecated deprecationReason }
fragment TypeRef on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } } }`

func main() {
	githubFile := flag.String("github", "", "convert this saved GitHub SDL instead of fetching it")
	gitlabFile := flag.String("gitlab", "", "convert this saved gitlab.com introspection answer instead of fetching it")
	out := flag.String("out", filepath.Join("internal", "git", "forgeschema"), "directory to write github.graphql and gitlab.graphql to")
	flag.Parse()
	if err := run(context.Background(), *githubFile, *gitlabFile, *out); err != nil {
		fmt.Fprintln(os.Stderr, "forgeschema refresh:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, githubFile, gitlabFile, out string) error {
	client := &http.Client{Timeout: 3 * time.Minute}
	githubSDL, err := source(ctx, client, githubFile, http.MethodGet, githubSchemaURL, nil)
	if err != nil {
		return fmt.Errorf("GitHub schema: %w", err)
	}
	body, err := json.Marshal(map[string]string{"query": introspectionQuery})
	if err != nil {
		return err
	}
	gitlabJSON, err := source(ctx, client, gitlabFile, http.MethodPost, gitlabGraphQL, body)
	if err != nil {
		return fmt.Errorf("gitlab.com schema: %w", err)
	}
	github, err := parser.ParseSchema(&ast.Source{Name: "github.graphql", Input: string(githubSDL)})
	if err != nil {
		return fmt.Errorf("parse GitHub's schema: %w", err)
	}
	gitlab, err := introspectionSchema(gitlabJSON)
	if err != nil {
		return fmt.Errorf("gitlab.com schema: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for name, doc := range map[string]*ast.SchemaDocument{"github.graphql": github, "gitlab.graphql": gitlab} {
		text, err := formatSchema(doc)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(out, name), text, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// source reads file, or fetches url when file is empty.
func source(ctx context.Context, client *http.Client, file, method, url string, body []byte) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSchemaBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	if len(data) > maxSchemaBytes {
		return nil, fmt.Errorf("%s %s: answer exceeds %d bytes", method, url, maxSchemaBytes)
	}
	return data, nil
}

// formatSchema prints doc without descriptions, its type and directive
// definitions sorted by name, and checks that the result loads as a
// schema.
func formatSchema(doc *ast.SchemaDocument) ([]byte, error) {
	byName := func(a, b *ast.Definition) int { return cmp.Compare(a.Name, b.Name) }
	slices.SortStableFunc(doc.Definitions, byName)
	slices.SortStableFunc(doc.Extensions, byName)
	slices.SortStableFunc(doc.Directives, func(a, b *ast.DirectiveDefinition) int { return cmp.Compare(a.Name, b.Name) })
	var buf bytes.Buffer
	formatter.NewFormatter(&buf, formatter.WithIndent("  "), formatter.WithoutDescription()).FormatSchemaDocument(doc)
	if _, err := validator.LoadSchema(validator.Prelude, &ast.Source{Input: buf.String()}); err != nil {
		return nil, fmt.Errorf("the written schema does not load: %w", err)
	}
	return buf.Bytes(), nil
}

// The introspection answer's shape.
type (
	introspection struct {
		Data *struct {
			Schema *introspectionSchemaRaw `json:"__schema"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	introspectionSchemaRaw struct {
		QueryType        *introspectionTypeRef `json:"queryType"`
		MutationType     *introspectionTypeRef `json:"mutationType"`
		SubscriptionType *introspectionTypeRef `json:"subscriptionType"`
		Types            []introspectionType   `json:"types"`
		Directives       []struct {
			Name         string               `json:"name"`
			Locations    []string             `json:"locations"`
			IsRepeatable bool                 `json:"isRepeatable"`
			Args         []introspectionInput `json:"args"`
		} `json:"directives"`
	}
	introspectionType struct {
		Kind          string                 `json:"kind"`
		Name          string                 `json:"name"`
		Fields        []introspectionField   `json:"fields"`
		InputFields   []introspectionInput   `json:"inputFields"`
		Interfaces    []introspectionTypeRef `json:"interfaces"`
		EnumValues    []introspectionEnum    `json:"enumValues"`
		PossibleTypes []introspectionTypeRef `json:"possibleTypes"`
	}
	introspectionField struct {
		Name              string               `json:"name"`
		Args              []introspectionInput `json:"args"`
		Type              introspectionTypeRef `json:"type"`
		IsDeprecated      bool                 `json:"isDeprecated"`
		DeprecationReason *string              `json:"deprecationReason"`
	}
	introspectionInput struct {
		Name              string               `json:"name"`
		Type              introspectionTypeRef `json:"type"`
		DefaultValue      *string              `json:"defaultValue"`
		IsDeprecated      bool                 `json:"isDeprecated"`
		DeprecationReason *string              `json:"deprecationReason"`
	}
	introspectionEnum struct {
		Name              string  `json:"name"`
		IsDeprecated      bool    `json:"isDeprecated"`
		DeprecationReason *string `json:"deprecationReason"`
	}
	introspectionTypeRef struct {
		Kind   string                `json:"kind"`
		Name   string                `json:"name"`
		OfType *introspectionTypeRef `json:"ofType"`
	}
)

// introspectionSchema converts an introspection answer to a schema
// document. The spec's own scalars, directives and introspection types
// are left to the validator's prelude.
func introspectionSchema(data []byte) (*ast.SchemaDocument, error) {
	var answer introspection
	if err := json.Unmarshal(data, &answer); err != nil {
		return nil, err
	}
	if len(answer.Errors) > 0 {
		return nil, fmt.Errorf("introspection refused: %s", answer.Errors[0].Message)
	}
	if answer.Data == nil || answer.Data.Schema == nil {
		return nil, errors.New("the answer holds no __schema")
	}
	raw := answer.Data.Schema
	prelude := validator.PreludeDocument()
	builtin := map[string]bool{}
	for _, def := range prelude.Definitions {
		builtin[def.Name] = true
	}
	for _, dir := range prelude.Directives {
		builtin["@"+dir.Name] = true
	}
	doc := &ast.SchemaDocument{}
	var roots ast.OperationTypeDefinitionList
	for op, ref := range map[ast.Operation]*introspectionTypeRef{ast.Query: raw.QueryType, ast.Mutation: raw.MutationType, ast.Subscription: raw.SubscriptionType} {
		if ref != nil {
			roots = append(roots, &ast.OperationTypeDefinition{Operation: op, Type: ref.Name})
		}
	}
	slices.SortFunc(roots, func(a, b *ast.OperationTypeDefinition) int { return cmp.Compare(a.Operation, b.Operation) })
	doc.Schema = ast.SchemaDefinitionList{{OperationTypes: roots}}
	for _, dir := range raw.Directives {
		if builtin["@"+dir.Name] {
			continue
		}
		def := &ast.DirectiveDefinition{Name: dir.Name, IsRepeatable: dir.IsRepeatable}
		for _, loc := range dir.Locations {
			def.Locations = append(def.Locations, ast.DirectiveLocation(loc))
		}
		args, err := inputValues(dir.Args)
		if err != nil {
			return nil, fmt.Errorf("directive @%s: %w", dir.Name, err)
		}
		def.Arguments = args
		doc.Directives = append(doc.Directives, def)
	}
	for _, t := range raw.Types {
		if builtin[t.Name] || strings.HasPrefix(t.Name, "__") {
			continue
		}
		def := &ast.Definition{Kind: ast.DefinitionKind(t.Kind), Name: t.Name}
		for _, iface := range t.Interfaces {
			def.Interfaces = append(def.Interfaces, iface.Name)
		}
		if t.Kind == string(ast.Union) {
			for _, member := range t.PossibleTypes {
				def.Types = append(def.Types, member.Name)
			}
		}
		for _, f := range t.Fields {
			args, err := inputValues(f.Args)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.Name, f.Name, err)
			}
			field := &ast.FieldDefinition{Name: f.Name, Type: f.Type.ast(), Directives: deprecated(f.IsDeprecated, f.DeprecationReason)}
			for _, arg := range args {
				field.Arguments = append(field.Arguments, &ast.ArgumentDefinition{Name: arg.Name, Type: arg.Type, DefaultValue: arg.DefaultValue, Directives: arg.Directives})
			}
			def.Fields = append(def.Fields, field)
		}
		inputs, err := inputValues(t.InputFields)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		for _, in := range inputs {
			def.Fields = append(def.Fields, &ast.FieldDefinition{Name: in.Name, Type: in.Type, DefaultValue: in.DefaultValue, Directives: in.Directives})
		}
		for _, v := range t.EnumValues {
			def.EnumValues = append(def.EnumValues, &ast.EnumValueDefinition{Name: v.Name, Directives: deprecated(v.IsDeprecated, v.DeprecationReason)})
		}
		doc.Definitions = append(doc.Definitions, def)
	}
	return doc, nil
}

// inputValues converts arguments or input fields, parsing each default
// value, which introspection gives as GraphQL source.
func inputValues(raw []introspectionInput) (ast.ArgumentDefinitionList, error) {
	var out ast.ArgumentDefinitionList
	for _, in := range raw {
		arg := &ast.ArgumentDefinition{Name: in.Name, Type: in.Type.ast(), Directives: deprecated(in.IsDeprecated, in.DeprecationReason)}
		if in.DefaultValue != nil {
			value, err := parseValue(*in.DefaultValue)
			if err != nil {
				return nil, fmt.Errorf("default of %s: %w", in.Name, err)
			}
			arg.DefaultValue = value
		}
		out = append(out, arg)
	}
	return out, nil
}

// parseValue parses one constant GraphQL value.
func parseValue(text string) (*ast.Value, error) {
	doc, err := parser.ParseQuery(&ast.Source{Input: "{ f(v: " + text + ") }"})
	if err != nil {
		return nil, err
	}
	return doc.Operations[0].SelectionSet[0].(*ast.Field).Arguments[0].Value, nil
}

// deprecated is the @deprecated directive a deprecated element carries.
func deprecated(isDeprecated bool, reason *string) ast.DirectiveList {
	if !isDeprecated {
		return nil
	}
	dir := &ast.Directive{Name: "deprecated"}
	if reason != nil {
		dir.Arguments = ast.ArgumentList{{Name: "reason", Value: &ast.Value{Kind: ast.StringValue, Raw: *reason}}}
	}
	return ast.DirectiveList{dir}
}

func (r introspectionTypeRef) ast() *ast.Type {
	switch r.Kind {
	case "NON_NULL":
		t := r.OfType.ast()
		t.NonNull = true
		return t
	case "LIST":
		return ast.ListType(r.OfType.ast(), nil)
	default:
		return ast.NamedType(r.Name, nil)
	}
}
