package safecopy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileCopiesAtomicallyAndLeavesNoTemp(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "file.env"), []byte("value"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := File(source, "nested/file.env", destination, "nested/file.env", 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "file.env"))
	if err != nil || string(data) != "value" {
		t.Fatalf("copied file = %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(destination, "nested", "file.env"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("copied mode = %v, %v", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(filepath.Join(destination, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), TempPrefix) {
			t.Fatalf("copy left a temp file behind: %s", entry.Name())
		}
	}
}

func TestFileRefusesNonRegularAndEscapingSources(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "outside"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "outside"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := File(source, "link", destination, "copied", 0o600); err == nil {
		t.Fatal("symlinked source copied")
	}
	if err := File(source, "../outside", destination, "copied", 0o600); err == nil {
		t.Fatal("escaping source copied")
	}
	if err := File(source, "link", destination, "../escaped", 0o600); err == nil {
		t.Fatal("escaping destination copied")
	}
	if _, err := os.Lstat(filepath.Join(destination, "copied")); !os.IsNotExist(err) {
		t.Fatalf("refused copy left a destination behind: %v", err)
	}
}

func TestValidateDestinationRefusesSymlinkedParents(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDestination(root, filepath.Join(root, "redirect", "file")); err == nil {
		t.Fatal("symlinked destination parent accepted")
	}
	if err := ValidateDestination(root, filepath.Join(external, "file")); err == nil {
		t.Fatal("destination outside the managed root accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "notadir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDestination(root, filepath.Join(root, "notadir", "file")); err == nil {
		t.Fatal("destination under a regular file accepted")
	}
	if err := ValidateDestination(root, filepath.Join(root, "fresh", "file")); err != nil {
		t.Fatalf("not-yet-created parent rejected: %v", err)
	}
}
