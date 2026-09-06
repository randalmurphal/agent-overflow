package keybindings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFrontendDefaultsSourceIsCheckedIn(t *testing.T) {
	got, err := os.ReadFile(filepath.FromSlash(FrontendDefaultsRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != FrontendDefaultsSource() {
		t.Fatal("frontend keybinding defaults are stale; run go generate ./internal/keybindings")
	}
}
