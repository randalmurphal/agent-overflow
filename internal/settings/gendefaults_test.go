package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The generated module is checked in so the frontend build needs no Go
// toolchain. This is the tripwire that keeps it honest.
func TestFrontendDefaultsSourceIsCheckedIn(t *testing.T) {
	path := filepath.FromSlash(FrontendDefaultsRelPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with `%s`)", path, err, FrontendDefaultsRegenCommand)
	}
	want := FrontendDefaultsSource()
	if string(got) != want {
		t.Fatalf("%s is stale; regenerate with `%s`", path, FrontendDefaultsRegenCommand)
	}
}

// Every json field is either emitted or denied with a reason. A new
// setting that is neither fails here, which is the point: whether the
// frontend gets a default is a decision, not an omission.
func TestFrontendDefaultsDenyListIsTotal(t *testing.T) {
	known := knownSettingsFieldNames()

	for name := range frontendDefaultsDenied {
		if _, ok := known[name]; !ok {
			t.Errorf("deny-list names %q, which is not a Settings json field", name)
		}
	}

	emitted := emittedFrontendDefaultNames(t)
	seen := make(map[string]struct{}, len(emitted))
	for _, name := range emitted {
		seen[name] = struct{}{}
	}
	for name := range known {
		_, isEmitted := seen[name]
		_, isDenied := frontendDefaultsDenied[name]
		switch {
		case isEmitted && isDenied:
			t.Errorf("field %q is both emitted and denied", name)
		case !isEmitted && !isDenied:
			t.Errorf("field %q is neither emitted nor denied: add it to frontendDefaultsDenied with a reason, or let it through", name)
		}
	}
}

// emittedFrontendDefaultNames is the field walk the generator performs,
// restated so the totality test compares against the same tag reading
// rather than parsing the rendered TypeScript.
func emittedFrontendDefaultNames(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(Settings{})
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := jsonFieldName(typ.Field(i))
		if name == "" {
			continue
		}
		if _, denied := frontendDefaultsDenied[name]; denied {
			continue
		}
		names = append(names, name)
	}
	return names
}
