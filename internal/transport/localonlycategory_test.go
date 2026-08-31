package transport

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// The category set is only closed if something closes it. Go's iota
// constants stop a typo but not an invention: `const CategoryWhatever
// LocalOnlyCategory = 11` compiles, and so does an entry tagged with it.
// These tests make the declared list the authority — the list being
// localOnlyCategoryNames, which is the only place every category has to
// appear by name.
//
// This is the shape the file already relies on elsewhere: methods_gen.go
// is generated from App's methods and methods_gen_test.go checks the
// classification against it. What was missing was a check on the
// classification's own vocabulary, which is how the header comment came
// to say six while the body used ten.

// TestLocalOnlyCategorySetIsClosed reads the LocalOnlyCategory constant
// block out of the source and requires it to match
// localOnlyCategoryNames exactly. A constant with no name row would
// stringify as an ordinal and read as a category in a failure message
// without ever having been described; a name row with no constant is a
// category nothing can be tagged with. Either way the "declared list"
// stops being one list.
func TestLocalOnlyCategorySetIsClosed(t *testing.T) {
	declared, explicitValues := parseCategoryConstants(t)

	if len(declared) == 0 {
		t.Fatal("no LocalOnlyCategory constants found in internalmethods.go; the scan has stopped matching and this gate is vacuous")
	}

	// The name-row check below reads values 1..N, which is only the
	// declared set if the block really is a contiguous iota starting at
	// 1. Assigning any constant an explicit value would break that
	// silently, so refuse it rather than trusting it.
	if len(explicitValues) != 1 || explicitValues[0] != declared[0] {
		t.Fatalf("the LocalOnlyCategory block must be a contiguous iota with exactly one explicit value, on the first constant; "+
			"explicit values found on %v (constants, in source order: %v)", explicitValues, declared)
	}

	if got, want := len(declared), len(localOnlyCategoryNames); got != want {
		t.Errorf("%d LocalOnlyCategory constants (%v) but %d rows in localOnlyCategoryNames; the two are the same list",
			got, declared, want)
	}
	for ordinal := 1; ordinal <= len(declared); ordinal++ {
		category := LocalOnlyCategory(ordinal)
		if _, named := localOnlyCategoryNames[category]; !named {
			t.Errorf("%s (ordinal %d) has no row in localOnlyCategoryNames; "+
				"a category nobody named stringifies as an ordinal and reads like a bug in every failure message",
				declared[ordinal-1], ordinal)
		}
	}
	for category := range localOnlyCategoryNames {
		if int(category) < 1 || int(category) > len(declared) {
			t.Errorf("localOnlyCategoryNames names ordinal %d, which no LocalOnlyCategory constant declares", category)
		}
	}
}

// TestEveryLocalOnlyMethodCarriesADeclaredCategory is the entry-side
// half: every name in the authored map is tagged, and tagged with
// something the set declares. The zero value is not a category, which is
// what makes an untagged entry fail rather than silently joining the
// first one.
func TestEveryLocalOnlyMethodCarriesADeclaredCategory(t *testing.T) {
	if len(localOnlyCategories) == 0 {
		t.Fatal("localOnlyCategories is empty; every gate over it would pass vacuously")
	}

	var untagged, unknown []string
	for method, category := range localOnlyCategories {
		switch {
		case category == 0:
			untagged = append(untagged, method)
		case localOnlyCategoryNames[category] == "":
			unknown = append(unknown, fmt.Sprintf("%s → %d", method, category))
		}
	}
	if len(untagged) > 0 {
		sort.Strings(untagged)
		t.Errorf("%d method(s) carry no category: %v\n\nTag each with the LocalOnlyCategory that put it in the set.",
			len(untagged), untagged)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("%d method(s) carry a category outside the declared set: %v\n\n"+
			"Either use a declared LocalOnlyCategory or declare a new one — with a doc comment and "+
			"a localOnlyCategoryNames row, which is what makes it declared.", len(unknown), unknown)
	}
}

// TestLocalOnlyMethodsMirrorsTheAuthoredMap pins the derivation. The
// exported set is what the dispatcher and every existing gate read, and
// the authored map is where a method is added — if the two ever came
// apart, a method could be classified and still answered, which is the
// one failure this whole file exists to prevent.
func TestLocalOnlyMethodsMirrorsTheAuthoredMap(t *testing.T) {
	if len(LocalOnlyMethods) != len(localOnlyCategories) {
		t.Fatalf("LocalOnlyMethods has %d entries, localOnlyCategories has %d", len(LocalOnlyMethods), len(localOnlyCategories))
	}
	for method := range localOnlyCategories {
		if !LocalOnlyMethods[method] {
			t.Errorf("%q is classified but LocalOnlyMethods does not contain it", method)
		}
	}
	for method, on := range LocalOnlyMethods {
		if !on {
			t.Errorf("LocalOnlyMethods[%q] is false; the derivation only ever writes true", method)
		}
		if _, ok := localOnlyCategories[method]; !ok {
			t.Errorf("LocalOnlyMethods contains %q with no row in localOnlyCategories", method)
		}
	}
}

// TestLocalOnlyCategoryOfAgreesWithMembership holds the accessor to the
// set rather than to its own copy of it.
func TestLocalOnlyCategoryOfAgreesWithMembership(t *testing.T) {
	for method := range LocalOnlyMethods {
		category, ok := LocalOnlyCategoryOf(method)
		if !ok {
			t.Errorf("LocalOnlyCategoryOf(%q) reports not-classified for a method in the set", method)
			continue
		}
		if localOnlyCategoryNames[category] == "" {
			t.Errorf("LocalOnlyCategoryOf(%q) = %d, which the declared set does not name", method, category)
		}
	}
	if category, ok := LocalOnlyCategoryOf("HighlightCode"); ok {
		t.Errorf("LocalOnlyCategoryOf reports HighlightCode as %v; it is deliberately wire-safe", category)
	}
}

// TestEveryDeclaredCategoryIsUsed catches the other drift direction: a
// category that classifies nothing is either a class we removed and left
// described, or a distinction that was never real. Both are worth
// noticing, and neither is worth keeping.
func TestEveryDeclaredCategoryIsUsed(t *testing.T) {
	used := make(map[LocalOnlyCategory]int, len(localOnlyCategoryNames))
	for _, category := range localOnlyCategories {
		used[category]++
	}
	for category, name := range localOnlyCategoryNames {
		if used[category] == 0 {
			t.Errorf("LocalOnlyCategory %q (%d) classifies no method; delete it, or say in its doc comment why it is held open", name, category)
		}
	}
}

// TestDeviceAccessSurfaceIsWholeAndLocalOnly is the tripwire for the one
// category where a single missing row is a credential-issuance call
// answered over the LAN. The generic gate in methods_gen_test.go catches
// an UNCLASSIFIED method, but a row moved to wireSafeMethods would pass
// it; naming the surface here means the whole set moves together or not
// at all.
func TestDeviceAccessSurfaceIsWholeAndLocalOnly(t *testing.T) {
	for _, method := range []string{
		"GetAccessOverview",
		"MintDevicePairing",
		"DevicePairingStatus",
		"ConfirmDevicePairing",
		"CancelDevicePairing",
		"RevokeAccessDevice",
		"RevokeAccessSession",
	} {
		if !LocalOnlyMethods[method] {
			t.Errorf("%q is not local-only; the device-access surface decides who reaches this backend at all", method)
			continue
		}
		category, ok := LocalOnlyCategoryOf(method)
		if !ok || category != CategoryDeviceAccess {
			t.Errorf("%q carries category %v, want CategoryDeviceAccess", method, category)
		}
	}
}

// parseCategoryConstants reads the LocalOnlyCategory const block out of
// internalmethods.go, in source order, and reports which constants carry
// an explicit value. Parsing the source rather than reflecting is the
// only way to see a constant that nothing references — which is exactly
// the constant this gate is looking for.
func parseCategoryConstants(t *testing.T) (names, explicitValues []string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "internalmethods.go", nil, 0)
	if err != nil {
		t.Fatalf("parse internalmethods.go: %v", err)
	}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// A grouped const carries its type forward when a spec omits
		// it, so track the last one seen rather than reading each spec
		// in isolation — which is the whole shape of this block, where
		// only the first constant names the type.
		var groupType string
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); ok {
				groupType = ident.Name
			}
			if groupType != "LocalOnlyCategory" {
				continue
			}
			for i, name := range value.Names {
				names = append(names, name.Name)
				if i < len(value.Values) {
					explicitValues = append(explicitValues, name.Name)
				}
			}
		}
	}
	return names, explicitValues
}
