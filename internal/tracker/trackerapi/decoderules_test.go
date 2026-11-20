package trackerapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The scrub of an answer body covers the text one JSON decode produces. A
// string field tagged with the `string` option is decoded twice, so a
// credential written with a doubled escape would reach it unseen. No type
// under internal/tracker may ask for that: structure cannot forbid a tag,
// so this test does.
func TestNoStringFieldIsDecodedTwice(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	fields := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			fields++
			if decodedTwice(field) {
				t.Errorf("%s: a string field with the json `string` option is decoded twice, past the answer scrub", fset.Position(field.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fields == 0 {
		t.Fatal("found no tagged field under internal/tracker")
	}
}

// decodedTwice reports a field of type string or *string whose json tag
// has the string option.
func decodedTwice(field *ast.Field) bool {
	typ := field.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); !ok || id.Name != "string" {
		return false
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return false
	}
	_, options, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
	for option := range strings.SplitSeq(options, ",") {
		if option == "string" {
			return true
		}
	}
	return false
}

func TestDecodedTwice(t *testing.T) {
	t.Parallel()
	for src, want := range map[string]bool{
		"V string `json:\"v,string\"`":            true,
		"V *string `json:\"v,omitempty,string\"`": true,
		"V string `json:\"v\"`":                   false,
		"V string `json:\"string\"`":              false,
		"V int `json:\"v,string\"`":               false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\ntype T struct { "+src+" }", 0)
		if err != nil {
			t.Fatal(err)
		}
		var got bool
		ast.Inspect(f, func(n ast.Node) bool {
			if field, ok := n.(*ast.Field); ok && field.Tag != nil {
				got = decodedTwice(field)
			}
			return true
		})
		if got != want {
			t.Errorf("%s: decodedTwice = %v, want %v", src, got, want)
		}
	}
}
