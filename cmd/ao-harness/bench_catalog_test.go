package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// The client cannot import the receiver. Check its advertised catalog from
// source so a new workload cannot pass unit tests then fail live preflight.
func TestBenchCatalogMatchesBackendCapabilities(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../../internal/harnessrpc/capabilities.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var advertised []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "harnessCapabilityWorkloadNames" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && literal.Kind == token.STRING {
				name, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				advertised = append(advertised, name)
			}
			return true
		})
	}
	var implemented []string
	for _, workload := range benchWorkloads() {
		implemented = append(implemented, workload.Name)
	}
	slices.Sort(advertised)
	slices.Sort(implemented)
	if !reflect.DeepEqual(advertised, implemented) {
		t.Fatalf("backend advertises %v; CLI implements %v", advertised, implemented)
	}
}
