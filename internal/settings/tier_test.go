package settings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A new setting must not ship without a tier decision: `settings:updated`
// carries the tier, and an unclassified key would silently broadcast as
// host-tier — the one class §6 puts behind step-up.
func TestEverySettingsKeyHasATier(t *testing.T) {
	for key := range knownSettingsFieldNames() {
		if key == schemaVersionKey {
			if _, classified := TierForKey(key); classified {
				t.Errorf("%s is file bookkeeping and must stay unclassified", key)
			}
			continue
		}
		if _, ok := TierForKey(key); !ok {
			t.Errorf("settings key %q has no tier in tierByKey", key)
		}
	}
}

// The reverse direction: a key removed from Settings must lose its tier row,
// or the map accumulates entries describing settings that no longer exist.
func TestEveryTierRowNamesALiveSettingsKey(t *testing.T) {
	known := knownSettingsFieldNames()
	for key := range tierByKey {
		if _, ok := known[key]; !ok {
			t.Errorf("tierByKey has %q, which Settings no longer declares", key)
		}
	}
}

// mutate is the persisted-write chokepoint (mutate.go). It stays the
// chokepoint only while it is the sole writeSparse caller: a second one would
// persist a change that announces nothing, and the client that did not issue
// it would keep rendering the old value indefinitely.
func TestOnlyMutateWritesSettings(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "writeSparse" {
					return true
				}
				callers = append(callers, fn.Name.Name)
				return true
			})
		}
	}
	slices.Sort(callers)
	callers = slices.Compact(callers)
	if !slices.Equal(callers, []string{"mutate"}) {
		t.Fatalf("writeSparse callers = %v, want exactly [mutate]", callers)
	}
}

func TestChangedKeysReportsOnlyMovedKeys(t *testing.T) {
	before := map[string]string{"fontSize": "13", "confirmDelete": "true", "gone": "1"}
	after := map[string]string{"fontSize": "15", "confirmDelete": "true", "added": "2"}
	got := changedKeys(before, after)
	want := []string{"added", "fontSize", "gone"}
	if !slices.Equal(got, want) {
		t.Fatalf("changedKeys = %v, want %v", got, want)
	}
}

// The schema stamp rides every save. Reporting it would make the first save
// after an upgrade look like a settings change on every attached client.
func TestChangedKeysIgnoresTheSchemaStamp(t *testing.T) {
	got := changedKeys(map[string]string{schemaVersionKey: "0"}, map[string]string{schemaVersionKey: "1"})
	if len(got) != 0 {
		t.Fatalf("changedKeys = %v, want none", got)
	}
}

func TestGroupByTierSplitsAWriteAcrossTiers(t *testing.T) {
	got := groupByTier([]string{"confirmDelete", "fontSize", "network"})
	want := []TierChange{
		{Tier: TierHost, Keys: []string{"network"}},
		{Tier: TierUser, Keys: []string{"confirmDelete"}},
		{Tier: TierDevice, Keys: []string{"fontSize"}},
	}
	if len(got) != len(want) {
		t.Fatalf("groupByTier = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Tier != want[i].Tier || !slices.Equal(got[i].Keys, want[i].Keys) {
			t.Fatalf("groupByTier[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
