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

// The scrub of an answer body covers the text one JSON decode produces.
// The json `string` tag option decodes a string value a second time when
// the field's kind is a string, so a credential written with a doubled
// escape would reach it unseen. Structure cannot forbid a tag, so this
// test does: under internal/tracker the option is allowed only on a field
// whose type is spelled as a built-in number or bool, which a second
// decode cannot turn into text. A named type is refused whatever its
// kind, since its spelling does not show it.
func TestStringTagOptionOnlyOnNumbers(t *testing.T) {
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
			if stringOptionRefused(field) {
				t.Errorf("%s: the json `string` option on a field that is not a built-in number or bool can decode a string twice, past the answer scrub", fset.Position(field.Pos()))
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

// numberKinds are the type spellings the string option is allowed on.
var numberKinds = map[string]bool{
	"bool": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true,
}

// stringOptionRefused reports a field whose json tag has the string
// option and whose type is not a built-in number or bool, or pointers to
// one.
func stringOptionRefused(field *ast.Field) bool {
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return true
	}
	_, options, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
	asked := false
	for option := range strings.SplitSeq(options, ",") {
		asked = asked || option == "string"
	}
	if !asked {
		return false
	}
	typ := field.Type
	for {
		switch t := typ.(type) {
		case *ast.StarExpr:
			typ = t.X
			continue
		case *ast.ParenExpr:
			typ = t.X
			continue
		}
		break
	}
	id, ok := typ.(*ast.Ident)
	return !ok || !numberKinds[id.Name]
}

func TestStringOptionRefused(t *testing.T) {
	t.Parallel()
	for src, want := range map[string]bool{
		"V string `json:\"v,string\"`":            true,
		"V *string `json:\"v,omitempty,string\"`": true,
		"V **string `json:\"v,string\"`":          true,
		"V (string) `json:\"v,string\"`":          true,
		"V Named `json:\"v,string\"`":             true,
		"V json.Number `json:\"v,string\"`":       true,
		"V []int `json:\"v,string\"`":             true,
		"V string `json:\"v\"`":                   false,
		"V string `json:\"string\"`":              false,
		"V int `json:\"v,string\"`":               false,
		"V *int64 `json:\"v,string,omitempty\"`":  false,
		"V bool `json:\"v,string\"`":              false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\ntype T struct { "+src+" }", 0)
		if err != nil {
			t.Fatal(err)
		}
		var got bool
		ast.Inspect(f, func(n ast.Node) bool {
			if field, ok := n.(*ast.Field); ok && field.Tag != nil {
				got = stringOptionRefused(field)
			}
			return true
		})
		if got != want {
			t.Errorf("%s: stringOptionRefused = %v, want %v", src, got, want)
		}
	}
}
