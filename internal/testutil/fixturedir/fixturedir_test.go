package fixturedir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAFixtureIsBuiltOnceWhereTheRunStarted builds a fixture first in a
// test that points TMPDIR at a directory of its own, as a test whose
// cleanup removes that directory does: the fixture is built once, in the
// directory the run made under the TMPDIR it started with, and is gone
// once the run ends.
func TestAFixtureIsBuiltOnceWhereTheRunStarted(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	builds := 0
	fixture := New("built", func(tb testing.TB, path string) {
		builds++
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			tb.Fatal(err)
		}
	})
	var path string
	code := Guard(func() int {
		own := filepath.Join(t.TempDir(), "own")
		if err := os.Mkdir(own, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", own)
		path = fixture.Path(t)
		if again := fixture.Path(t); again != path {
			t.Errorf("second Path %q; want %q", again, path)
		}
		return 0
	})
	if code != 0 || builds != 1 {
		t.Fatalf("Guard = %d with %d builds; want 0 and one build", code, builds)
	}
	if got := filepath.Dir(filepath.Dir(path)); got != base {
		t.Fatalf("the fixture was built at %s, under %s; want it under %s, the TMPDIR the run started with", path, got, base)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture directory after the run: %v; want it removed", err)
	}
}
