// Package olderrelease is the release a paired computer may still run
// (v0.0.19) as this build's tests and harness stand it up: the bound App
// methods that release has, frozen in methods.txt from the tag's
// generated method table. What this build binds and the release does not
// is derived from it on both sides: the harness's older computer
// (olderComputerSurface in main_harness.go) and the frontend's
// (frontend/src/test/helpers/olderComputer.ts). A new bound method needs
// no edit to either.
//
// `make older-release TAG=<tag>` writes methods.txt from a tag
// (./gen); the test checks it against the tag's table whenever the tag is
// reachable.
package olderrelease

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

//go:embed methods.txt
var methodsFile string

// Release is the frozen release: its tag and its bound App methods,
// sorted.
type Release struct {
	Tag     string
	Methods []string
}

// Has reports whether the release binds method name.
func (r Release) Has(name string) bool {
	_, found := slices.BinarySearch(r.Methods, name)
	return found
}

// Frozen is the release methods.txt holds. A file that does not parse is
// a build defect, which the package's test fails on.
func Frozen() Release {
	r, err := Parse(methodsFile)
	if err != nil {
		panic(fmt.Sprintf("olderrelease: methods.txt: %v", err))
	}
	return r
}

// tagPrefix opens the header line that names the release's tag.
const tagPrefix = "# tag "

// Parse reads a methods file: a "# tag <tag>" line, other "#" lines, and
// one method name a line, sorted, each once.
func Parse(text string) (Release, error) {
	var r Release
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, tagPrefix):
			r.Tag = strings.TrimPrefix(line, tagPrefix)
		case strings.HasPrefix(line, "#"):
		case !token.IsIdentifier(line):
			return Release{}, fmt.Errorf("%q is not a method name", line)
		case len(r.Methods) > 0 && line <= r.Methods[len(r.Methods)-1]:
			return Release{}, fmt.Errorf("%s is out of order or named twice", line)
		default:
			r.Methods = append(r.Methods, line)
		}
	}
	switch {
	case r.Tag == "":
		return Release{}, fmt.Errorf("no %q line names the release", strings.TrimSpace(tagPrefix))
	case len(r.Methods) == 0:
		return Release{}, fmt.Errorf("no method of %s is named", r.Tag)
	}
	return r, nil
}

// Format is the methods file of r.
func Format(r Release) string {
	var b strings.Builder
	b.WriteString("# The bound App methods of a release a paired computer may run, one a line.\n")
	b.WriteString("# Written by `make older-release TAG=" + r.Tag + "` from the tag's internal/transport/methods_gen.go; do not edit.\n")
	b.WriteString(tagPrefix + r.Tag + "\n")
	for _, m := range r.Methods {
		b.WriteString(m + "\n")
	}
	return b.String()
}

// TableNames are the method names of the GeneratedMethods table in src,
// the source of an internal/transport/methods_gen.go, sorted.
func TableNames(src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "methods_gen.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse the method table: %w", err)
	}
	var table *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if name.Name == "GeneratedMethods" && i < len(vs.Values) {
					table, _ = vs.Values[i].(*ast.CompositeLit)
				}
			}
		}
	}
	if table == nil {
		return nil, fmt.Errorf("the source has no GeneratedMethods table")
	}
	var names []string
	for _, elt := range table.Elts {
		row, ok := elt.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("a GeneratedMethods row is not a composite literal")
		}
		name := ""
		for _, field := range row.Elts {
			kv, ok := field.(*ast.KeyValueExpr)
			if key, isIdent := kv.Key.(*ast.Ident); !ok || !isIdent || key.Name != "Name" {
				continue
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return nil, fmt.Errorf("a GeneratedMethods row's Name is not a string literal")
			}
			if name, err = strconv.Unquote(lit.Value); err != nil {
				return nil, fmt.Errorf("a GeneratedMethods row's Name %s: %w", lit.Value, err)
			}
		}
		if name == "" {
			return nil, fmt.Errorf("a GeneratedMethods row names no method")
		}
		names = append(names, name)
	}
	slices.Sort(names)
	if dup := slices.Compact(slices.Clone(names)); len(dup) != len(names) {
		return nil, fmt.Errorf("the GeneratedMethods table names a method twice")
	}
	return names, nil
}
