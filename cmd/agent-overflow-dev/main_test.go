package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestShouldSkipDirMatchesDevModeExcludes(t *testing.T) {
	watch := testWatchConfig()
	for _, name := range []string{".git", "node_modules", "frontend", "bin", ".claude", ".playwright-mcp"} {
		if !shouldSkipDir(watch, name, name) {
			t.Fatalf("shouldSkipDir(%q) = false, want true", name)
		}
	}
	if shouldSkipDir(watch, ".", ".") {
		t.Fatal("root directory was skipped")
	}
	if shouldSkipDir(watch, "internal", "internal") {
		t.Fatal("normal source directory was skipped")
	}
}

func TestWatchConfigLoadsFromWailsDevMode(t *testing.T) {
	watch, err := loadWatchConfig(repoRoot(t))
	if err != nil {
		t.Fatalf("loadWatchConfig() error = %v", err)
	}
	for _, name := range []string{".git", "node_modules", "frontend", "bin", ".claude", ".playwright-mcp"} {
		if !shouldSkipDir(watch, name, name) {
			t.Fatalf("loaded watch config does not skip %q", name)
		}
	}
	for _, rel := range []string{"main.go", "internal/example.ts", "internal/example.js"} {
		if !shouldTrackFile(watch, rel) {
			t.Fatalf("loaded watch config does not track %q", rel)
		}
	}
	if shouldTrackFile(watch, "internal/readme.md") {
		t.Fatal("loaded watch config tracks markdown")
	}
	if !watch.useGitIgnore {
		t.Fatal("loaded watch config does not honor git_ignore")
	}
}

func TestTakeSnapshotTracksBackendExtensionsAndIgnoresFrontend(t *testing.T) {
	root := t.TempDir()
	watch := testWatchConfig()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "internal/example.ts", "export const x = 1;\n")
	writeFile(t, root, "internal/example.js", "export const x = 1;\n")
	writeFile(t, root, "internal/readme.md", "# ignored\n")
	writeFile(t, root, "frontend/src/main.ts", "ignored\n")
	writeFile(t, root, "bin/generated.go", "ignored\n")
	writeFile(t, root, ".claude/worktrees/agent-1/main.go", "ignored\n")

	got, err := takeSnapshot(root, watch)
	if err != nil {
		t.Fatalf("takeSnapshot() error = %v", err)
	}
	keys := sortedSnapshotKeys(got)
	want := []string{
		filepath.FromSlash("internal/example.js"),
		filepath.FromSlash("internal/example.ts"),
		"main.go",
	}
	if len(keys) != len(want) {
		t.Fatalf("snapshot keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("snapshot keys = %v, want %v", keys, want)
		}
	}
}

func TestChangedSinceDetectsContentReplacementWithSameSize(t *testing.T) {
	root := t.TempDir()
	watch := testWatchConfig()
	target := writeFile(t, root, "main.go", "abc\n")
	before, err := takeSnapshot(root, watch)
	if err != nil {
		t.Fatalf("takeSnapshot before: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(target, []byte("xyz\n"), 0644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	_, changed, err := changedSince(root, watch, before)
	if err != nil {
		t.Fatalf("changedSince() error = %v", err)
	}
	if !changed {
		t.Fatal("changedSince() changed = false, want true")
	}
}

func TestWaitForHTTPReturnsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForHTTP(ctx, "http://127.0.0.1:1", time.Second)
	if err == nil {
		t.Fatal("waitForHTTP() error = nil, want cancellation error")
	}
}

func TestEnvIntRejectsPartialNumbers(t *testing.T) {
	t.Setenv("WAILS_VITE_PORT", "9245abc")
	if got := envInt("WAILS_VITE_PORT", 1111); got != 1111 {
		t.Fatalf("envInt() = %d, want fallback", got)
	}
}

func testWatchConfig() watchConfig {
	return watchConfig{
		ignoredDirs: map[string]struct{}{
			".git":            {},
			"node_modules":    {},
			"frontend":        {},
			"bin":             {},
			".claude":         {},
			".playwright-mcp": {},
		},
		watchedExtensions: map[string]struct{}{
			".go": {},
			".js": {},
			".ts": {},
		},
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func writeFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

func sortedSnapshotKeys(s snapshot) []string {
	keys := make([]string, 0, len(s))
	for key := range s {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
