// refresh rewrites the Linear GraphQL schema that lineartest embeds and
// the linear package's tests check every document against: Linear's
// public SDL, written without descriptions and with its definitions
// sorted by name, so a refresh diffs by what changed. Deprecation markers
// stay. It sends no key.
// Tests never run it: refresh by hand on the host network and commit the
// result.
//
//	make linear-schema
//	go run ./internal/tracker/linear/refresh [-in FILE] [-out FILE]
//
// -in names a saved copy of the SDL to convert instead of fetching it.
package main

import (
	"bytes"
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
)

const (
	schemaURL = "https://raw.githubusercontent.com/linear/linear/master/packages/sdk/src/schema.graphql"
	// maxSchemaBytes bounds the download; the SDL is about 1.6 MB.
	maxSchemaBytes = 32 << 20
)

func main() {
	in := flag.String("in", "", "convert this saved SDL instead of fetching it")
	out := flag.String("out", filepath.Join("internal", "tracker", "linear", "lineartest", "testdata", "schema.graphql"), "file to write")
	flag.Parse()
	if err := run(context.Background(), *in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "linear schema refresh:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, in, out string) error {
	sdl, err := source(ctx, in)
	if err != nil {
		return fmt.Errorf("read the schema: %w", err)
	}
	doc, err := parser.ParseSchema(&ast.Source{Name: "schema.graphql", Input: string(sdl)})
	if err != nil {
		return fmt.Errorf("parse Linear's schema: %w", err)
	}
	text, err := formatSchema(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, text, 0o644)
}

// source reads file, or fetches the published SDL when file is empty.
func source(ctx context.Context, file string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, schemaURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSchemaBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", schemaURL, resp.Status)
	}
	if len(data) > maxSchemaBytes {
		return nil, fmt.Errorf("GET %s: answer exceeds %d bytes", schemaURL, maxSchemaBytes)
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
