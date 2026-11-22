//go:build unix

package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveDeletesTheFileAndIsDoneWhenItIsGone(t *testing.T) {
	t.Parallel()
	file := keyFile(t, testKey, 0o700, 0o600)
	if err := file.Remove(); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if _, err := os.Lstat(file.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key file is still there: %v", err)
	}
	if err := file.Remove(); err != nil {
		t.Fatalf("second Remove = %v, want nil", err)
	}
	if err := testFile(t).Remove(); err != nil {
		t.Fatalf("Remove without a folder = %v, want nil", err)
	}
	if status := file.Inspect(); status.State != StateMissing {
		t.Fatalf("Inspect after Remove = %+v, want missing", status)
	}
}

// A symlink at the key file's name is removed itself; what it points at is
// left alone.
func TestRemoveDeletesASymlinkNotItsTarget(t *testing.T) {
	t.Parallel()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte(testKey), 0o644); err != nil {
		t.Fatal(err)
	}
	file := testFile(t)
	if err := os.Mkdir(filepath.Dir(file.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, file.Path()); err != nil {
		t.Fatal(err)
	}
	if err := file.Remove(); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if _, err := os.Lstat(file.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the symlink is still there: %v", err)
	}
	got, err := os.ReadFile(elsewhere)
	if err != nil || string(got) != testKey {
		t.Fatalf("the symlink's target = %q, %v; want it untouched", got, err)
	}
	if got := mode(t, elsewhere); got != 0o644 {
		t.Fatalf("the symlink's target mode changed to %o", got)
	}
}

// A symlinked folder is refused, so nothing in the folder it points at is
// removed.
func TestRemoveRefusesASymlinkedFolder(t *testing.T) {
	t.Parallel()
	realFolder := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	file := testFile(t)
	target := filepath.Join(realFolder, filepath.Base(file.Path()))
	if err := os.WriteFile(target, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFolder, filepath.Dir(file.Path())); err != nil {
		t.Fatal(err)
	}
	var refusal *RefusedError
	if err := file.Remove(); !errors.As(err, &refusal) || refusal.Reason != "its folder is a symlink" {
		t.Fatalf("Remove = %v, want refusal %q", err, "its folder is a symlink")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != testKey {
		t.Fatalf("the file behind the symlinked folder = %q, %v; want it untouched", got, err)
	}
}

func TestRemoveRefusesAFolderAtTheFilesName(t *testing.T) {
	t.Parallel()
	file := testFile(t)
	if err := os.MkdirAll(file.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	var refusal *RefusedError
	if err := file.Remove(); !errors.As(err, &refusal) || refusal.Reason != "it is not a regular file" {
		t.Fatalf("Remove = %v, want refusal %q", err, "it is not a regular file")
	}
	if info, err := os.Stat(file.Path()); err != nil || !info.IsDir() {
		t.Fatalf("the folder at the file's name is gone: %v", err)
	}
}
