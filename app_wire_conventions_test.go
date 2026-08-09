package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Server-push payloads are addressed by the ENTITY they describe — a cwd, a PR
// key, a thread id — never by the subscription that happens to be listening.
// A subscription-keyed event forces every consumer to keep a private copy
// filtered by its own handle, which is exactly how two panes on one worktree
// ended up disagreeing about whether there was anything to commit (audit
// 2026-08-08). Entity keying is what lets one observation heal every consumer.
//
// The subscription id itself stays legitimate on the RPC RESULT that hands out
// the unsubscribe/cleanup handle (GitStatusSubscriptionResult.ID) — that is a
// per-caller lease, not an address. So the rule is about the wire NAME:
// nothing that gets serialized may be called "subscriptionId".
//
// Doctrine: internal/transport/AGENTS.md → "Events Are Entity-Keyed";
// frontend/CLAUDE.md → "State Boundaries".

// goSourceRoots mirrors GO_PACKAGE_ROOTS in the Makefile: the root package,
// cmd/, and internal/. A new Go tree added to the build has to be added here
// too, or the convention stops being enforced where it was added.
var goSourceRoots = []string{".", "cmd", "internal"}

// bannedWireNames are the serialized names no field may carry, compared
// lowercased so SubscriptionId / subscriptionID / SubscriptionID all trip
// them. The set is the same idea spelled the ways a future stream is
// plausibly named — a tripwire, not an exhaustive taxonomy: anything that
// addresses a payload by the LEASE rather than by the entity belongs here.
var bannedWireNames = map[string]string{
	"subscriptionid": "subscription handle",
	"subid":          "subscription handle",
	"streamid":       "stream handle",
	"handleid":       "lease handle",
	"watcherid":      "watcher handle",
}

func TestWirePayloadsAreEntityKeyedNotSubscriptionKeyed(t *testing.T) {
	for _, file := range collectGoSources(t) {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			structType, ok := node.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			// An untagged field on a serialized struct still goes over the wire
			// under its Go name, so untagged fields are checked too — but only
			// on structs that are demonstrably wire shapes (at least one json
			// tag). Go-internal bookkeeping that tracks subscription handles by
			// field name is not this rule's business.
			wireShape := structHasJSONTag(structType)
			for _, field := range structType.Fields.List {
				name, tagged, serialized := jsonFieldName(field)
				if !serialized || (!tagged && !wireShape) {
					continue
				}
				kind, banned := bannedWireNames[strings.ToLower(name)]
				if !banned {
					continue
				}
				t.Errorf(
					"%s:%d serializes %q (a %s) — events are keyed by their entity (cwd, PR key, thread id), "+
						"never by a per-caller handle; see internal/transport/AGENTS.md",
					file, fileSet.Position(field.Pos()).Line, name, kind,
				)
			}
			return true
		})
	}
}

func structHasJSONTag(structType *ast.StructType) bool {
	for _, field := range structType.Fields.List {
		if _, ok := jsonTag(field); ok {
			return true
		}
	}
	return false
}

// jsonFieldName resolves the name a field serializes under: the json tag's name
// when it sets one, otherwise the Go field name (encoding/json's default).
// `tagged` reports whether a json tag was present; `serialized` is false for
// embedded fields and `json:"-"`, which carry no name of their own.
func jsonFieldName(field *ast.Field) (name string, tagged bool, serialized bool) {
	if len(field.Names) == 0 {
		return "", false, false
	}
	goName := field.Names[0].Name
	tag, ok := jsonTag(field)
	if !ok {
		return goName, false, true
	}
	tagName, _, _ := strings.Cut(tag, ",")
	switch tagName {
	case "-":
		return "", true, false
	case "":
		return goName, true, true
	default:
		return tagName, true, true
	}
}

func jsonTag(field *ast.Field) (string, bool) {
	if field.Tag == nil {
		return "", false
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		// The parser only produces a valid string literal here; an unquotable
		// tag would be a silent hole, so fail loudly rather than skipping.
		panic("unquotable struct tag literal: " + field.Tag.Value)
	}
	return reflect.StructTag(raw).Lookup("json")
}

// collectGoSources returns every .go file the Makefile's package roots build,
// tests included — a fixture that reintroduces the shape is as much a
// regression as production code doing it.
func collectGoSources(t *testing.T) []string {
	t.Helper()
	var files []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		files = append(files, entry.Name())
	}
	for _, root := range goSourceRoots {
		if root == "." {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no Go sources found; the convention would pass vacuously")
	}
	return files
}
