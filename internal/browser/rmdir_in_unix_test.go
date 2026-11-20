//go:build linux || darwin

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// A room's leftover entry is removed only as an empty directory: whatever
// else its name holds by then stays, and removeMadeIn reports no failure
// for it.
func TestRemovingAMadeDirectoryTakesOnlyAnEmptyDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		put   func(t *testing.T, path string)
		stays bool
	}{
		{name: "an empty directory", put: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a file", stays: true, put: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not AO's"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory holding a file", stays: true, put: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "kept"), []byte("not AO's"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a link to a directory", stays: true, put: func(t *testing.T, path string) {
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			path := filepath.Join(dir, "room")
			tc.put(t, path)
			// The identity removeMadeIn compares is the entry's own, so the
			// removal is what decides.
			held, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := removeMadeIn(parent, "room", held); err != nil {
				t.Fatalf("removeMadeIn: %v", err)
			}
			_, err = os.Lstat(path)
			if stays := err == nil; stays != tc.stays {
				t.Fatalf("the entry stays = %v, want %v (lstat: %v)", stays, tc.stays, err)
			}
		})
	}
	t.Run("a name that is a path", func(t *testing.T) {
		parent, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		if err := rmdirIn(parent, "a/b"); err == nil {
			t.Fatal("rmdirIn took a path")
		}
	})
}
