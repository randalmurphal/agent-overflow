package appdirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootAppendsDirName(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if filepath.Base(root) != DirName {
		t.Fatalf("Root = %q, want basename %q", root, DirName)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("UserConfigDir unavailable in this environment: %v", err)
	}
	if root != filepath.Join(base, DirName) {
		t.Fatalf("Root = %q, want %q", root, filepath.Join(base, DirName))
	}
}
