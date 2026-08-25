package store

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// frozenMigrationGuidance is the one sentence every failure in this file ends
// with. Written once so a hash mismatch and a missing freeze entry cannot give
// two different accounts of the same rule.
const frozenMigrationGuidance = "shipped migration SQL changed — shipped migrations are immutable; " +
	"add a new migration instead. If you are certain this migration has never shipped, update the frozen hash."

// frozenMigrationSQL pins the sha256 of the FINAL SQL text of every migration
// whose text is DERIVED at package init from an earlier migration's text
// (the mustReplaceOnce / mustReplaceEvery / mustCutFrom family in migrate.go).
//
// A derived migration is the store's one place where editing source A silently
// rewrites already-shipped migration B: v43's rebuild is v39's text with a
// substitution, v56's is v44's, and so on down the chain. A database that
// applied B before the edit and one that applies it after then hold different
// schemas from the same version number — a divergence nothing in the chain can
// detect or repair, because the version row says the migration ran.
//
// The hashes are captured from the shipped tree. TestEveryDerivedMigrationIsFrozen
// is what keeps this map complete: a NEW migration built with the derivation
// helpers fails until its hash is added here.
var frozenMigrationSQL = map[int]string{
	28: "1ae75644c52742546f4d3f2d639abb4d91f2985b8c66367945ddf4d60de9758d",
	31: "07eb1088d2f304160b95cdda783af7efa204a7098edcef676373a1d31f8828dc",
	34: "b4ff11a9eccb25d899f407a9c5704c535d379440ae35f856df1f516d3dc029f9",
	39: "d77e162c0f548400ad9b876dcd4ed4d42b23615b772c782792ad732d8ad3215c",
	43: "34326851328903e1c01c0b001b9b92a2d2c68c667a8ed2b628003355931a7958",
	44: "a791f300012ab9d7a7e5bf238d5227bc510cadd599b6a18d7c000f9a28947368",
	45: "d7d091f4697bc6e3ac42be97468dc04662a0ae293bf79820ba15a6cb48f2746a",
	48: "ab1f8c0b914d3617cb97c897a2e19f8269e9764036adf6da7a6e2e978355078a",
	56: "40580b3b011a0dbba688b6a6a6b15a12130977e428e8506e6c39a8f1f659ae27",
	57: "e80e155278ebd5731667affaff0469aed4d4eff6092f142959d5ded2d0baba5e",
}

// TestShippedMigrationSQLIsFrozen hashes the final SQL of every frozen
// migration — the text package init actually produced, derivations applied —
// and compares it against the hash captured when that migration shipped.
func TestShippedMigrationSQLIsFrozen(t *testing.T) {
	byVersion := migrationsByVersion()

	for _, version := range sortedVersions(frozenMigrationSQL) {
		m, ok := byVersion[version]
		if !ok {
			t.Errorf("migration v%d is frozen but no longer exists in the chain: %s", version, frozenMigrationGuidance)
			continue
		}
		want := frozenMigrationSQL[version]
		if got := sha256Hex(m.SQL); got != want {
			t.Errorf("migration v%d (%s): %s\n  frozen sha256: %s\n  current sha256: %s",
				version, m.Name, frozenMigrationGuidance, want, got)
		}
	}
}

// TestEveryDerivedMigrationIsFrozen is the completeness half of the freeze: it
// parses this package's source, works out which migrations take their SQL from
// a derivation helper (directly or through another derived declaration), and
// fails if any of them is missing from frozenMigrationSQL.
//
// Mechanical on purpose. A freeze list maintained by hand only protects the
// migrations somebody remembered to add to it, and the whole hazard is that
// the derivation is invisible at the migration's own entry — `SQL:
// rebuildWorkItemRetryReasonsV56SQL` looks exactly like a const.
func TestEveryDerivedMigrationIsFrozen(t *testing.T) {
	byVersion := migrationsByVersion()

	derived := derivedMigrationVersions(t)
	if len(derived) == 0 {
		t.Fatal("no derived migrations detected — the detector has drifted from migrate.go; " +
			"if the derivation helpers are gone, delete this test with them")
	}

	for _, version := range derived {
		if _, ok := frozenMigrationSQL[version]; ok {
			continue
		}
		m := byVersion[version]
		t.Errorf("migration v%d (%s) derives its SQL from an earlier migration but is not in frozenMigrationSQL.\n"+
			"Add it so a later edit to the text it derives from cannot silently rewrite it:\n"+
			"\t%d: %q,",
			version, m.Name, version, sha256Hex(m.SQL))
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func migrationsByVersion() map[int]Migration {
	byVersion := make(map[int]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	return byVersion
}

func sortedVersions[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// derivationHelpers are the functions that build one migration's SQL out of
// another's. Any declaration whose initializer reaches one of these — through
// any number of intermediate declarations — carries a shipped migration's text
// inside it.
var derivationHelpers = map[string]bool{
	"mustReplaceOnce":  true,
	"mustReplaceEvery": true,
	"mustCutFrom":      true,
}

// derivedMigrationVersions returns, sorted, the version of every entry in the
// `migrations` slice whose SQL expression reaches a derivation helper.
func derivedMigrationVersions(t *testing.T) []int {
	t.Helper()

	fset := token.NewFileSet()
	files := parsePackageSource(t, fset)

	// Pass 1: every top-level declaration name paired with its initializer.
	inits := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.VAR && gen.Tok != token.CONST) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, name := range vs.Names {
					inits[name.Name] = vs.Values[i]
				}
			}
		}
	}

	// Pass 2: fixpoint over "this initializer reaches a derivation helper, or
	// references a name that does".
	derivedNames := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, expr := range inits {
			if derivedNames[name] {
				continue
			}
			if exprIsDerived(expr, derivedNames) {
				derivedNames[name] = true
				changed = true
			}
		}
	}

	// Pass 3: walk the migrations slice literal.
	migrationsLit := findCompositeLit(files, "migrations")
	if migrationsLit == nil {
		t.Fatal("could not find the `migrations` slice literal in this package's source")
	}

	var versions []int
	for _, elt := range migrationsLit.Elts {
		entry, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		version, sqlExpr, ok := migrationEntryFields(entry)
		if !ok || sqlExpr == nil {
			continue
		}
		if exprIsDerived(sqlExpr, derivedNames) {
			versions = append(versions, version)
		}
	}
	sort.Ints(versions)
	return versions
}

// exprIsDerived reports whether expr calls a derivation helper or mentions a
// name already known to be derived.
func exprIsDerived(expr ast.Expr, derivedNames map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.CallExpr:
			if ident, ok := node.Fun.(*ast.Ident); ok && derivationHelpers[ident.Name] {
				found = true
				return false
			}
		case *ast.Ident:
			if derivedNames[node.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// migrationEntryFields pulls Version and the SQL expression out of one
// `Migration{...}` literal.
func migrationEntryFields(entry *ast.CompositeLit) (version int, sqlExpr ast.Expr, ok bool) {
	for _, field := range entry.Elts {
		kv, isKV := field.(*ast.KeyValueExpr)
		if !isKV {
			continue
		}
		key, isIdent := kv.Key.(*ast.Ident)
		if !isIdent {
			continue
		}
		switch key.Name {
		case "Version":
			lit, isLit := kv.Value.(*ast.BasicLit)
			if !isLit || lit.Kind != token.INT {
				continue
			}
			n, err := strconv.Atoi(lit.Value)
			if err != nil {
				continue
			}
			version, ok = n, true
		case "SQL":
			sqlExpr = kv.Value
		}
	}
	return version, sqlExpr, ok
}

func findCompositeLit(files []*ast.File, name string) *ast.CompositeLit {
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, isVS := spec.(*ast.ValueSpec)
				if !isVS || len(vs.Names) != 1 || vs.Names[0].Name != name || len(vs.Values) != 1 {
					continue
				}
				if lit, isLit := vs.Values[0].(*ast.CompositeLit); isLit {
					return lit
				}
			}
		}
	}
	return nil
}

// parsePackageSource parses this package's non-test .go files. The test runs
// with the package directory as its working directory, which is what makes the
// bare glob correct.
func parsePackageSource(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package source: %v", err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no package source files found next to the test")
	}
	return files
}
