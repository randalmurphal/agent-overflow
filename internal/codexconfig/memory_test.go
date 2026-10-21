package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex's memories are on only when its config turns the feature on.
func TestMemoriesFollowTheConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, text := range map[string]string{
		"on.toml":     "[features]\nmemories = true\n",
		"dotted.toml": "features.memories = true\n",
		"off.toml":    "[features]\nmemories = false\n",
		"silent.toml": "model = \"gpt-5\"\n",
		"broken.toml": "[features\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]bool{"on.toml": true, "dotted.toml": true, "off.toml": false, "silent.toml": false, "absent.toml": false} {
		if got, err := New(filepath.Join(dir, name)).MemoriesOn(); err != nil || got != want {
			t.Errorf("%s: on = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := New(filepath.Join(dir, "broken.toml")).MemoriesOn(); err == nil || !strings.Contains(err.Error(), "broken.toml") {
		t.Errorf("a malformed config: %v", err)
	}
}
