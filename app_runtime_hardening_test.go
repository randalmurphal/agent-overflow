package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInitStoresRepairsAppOwnedPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	configRoot := t.TempDir()
	t.Setenv("AGENT_OVERFLOW_DEBUG", "provider")

	dbDir := filepath.Join(configRoot, "agent-overflow")
	for _, dir := range []string{
		dbDir,
		filepath.Join(dbDir, "logs"),
		filepath.Join(dbDir, "attachments"),
		filepath.Join(dbDir, "attachments", "thread-a"),
		filepath.Join(dbDir, "replay"),
		filepath.Join(dbDir, "ui-trace"),
		filepath.Join(dbDir, "ui-trace", "bookmarks"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	dbPath := filepath.Join(dbDir, "agent-overflow.db")
	if err := os.WriteFile(dbPath, []byte{}, 0o644); err != nil {
		t.Fatalf("seed db file: %v", err)
	}
	for _, file := range []string{
		filepath.Join(dbDir, "logs", "old.ndjson"),
		filepath.Join(dbDir, "attachments", "thread-a", "old.png"),
		filepath.Join(dbDir, "replay", "thread-a.jsonl"),
		filepath.Join(dbDir, "ui-trace", "ui-render.jsonl"),
		filepath.Join(dbDir, "ui-trace", "bookmarks", "bug-report.jsonl"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(file), err)
		}
		if err := os.WriteFile(file, []byte("seed\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", file, err)
		}
	}

	app := NewApp()
	// Inject the data-dir root so the test is deterministic across OSes;
	// os.UserConfigDir() ignores XDG on macOS, so an env override wouldn't
	// redirect it there.
	app.userConfigDirOverride = configRoot
	gotDir, st, err := app.initStores()
	if err != nil {
		t.Fatalf("initStores: %v", err)
	}
	defer st.Close()
	if app.logger != nil {
		defer app.logger.Close()
	}
	if gotDir != dbDir {
		t.Fatalf("dbDir = %q, want %q", gotDir, dbDir)
	}

	for _, dir := range []string{
		dbDir,
		filepath.Join(dbDir, "logs"),
		filepath.Join(dbDir, "attachments"),
		filepath.Join(dbDir, "attachments", "thread-a"),
		filepath.Join(dbDir, "replay"),
		filepath.Join(dbDir, "ui-trace"),
		filepath.Join(dbDir, "ui-trace", "bookmarks"),
	} {
		assertAppMode(t, dir, 0o700)
	}
	assertAppMode(t, dbPath, 0o600)
	for _, file := range []string{
		filepath.Join(dbDir, "logs", "old.ndjson"),
		filepath.Join(dbDir, "attachments", "thread-a", "old.png"),
		filepath.Join(dbDir, "replay", "thread-a.jsonl"),
		filepath.Join(dbDir, "ui-trace", "ui-render.jsonl"),
		filepath.Join(dbDir, "ui-trace", "bookmarks", "bug-report.jsonl"),
	} {
		assertAppMode(t, file, 0o600)
	}

	logFiles, err := filepath.Glob(filepath.Join(dbDir, "logs", "provider-events-*.ndjson"))
	if err != nil {
		t.Fatalf("glob logs: %v", err)
	}
	if len(logFiles) != 1 {
		t.Fatalf("provider event log count = %d, want 1 (%v)", len(logFiles), logFiles)
	}
	assertAppMode(t, logFiles[0], 0o600)
}

func TestPrepareAppSensitiveFileRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on some Windows hosts")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	link := filepath.Join(dir, "agent-overflow.db")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := prepareAppSensitiveFile(link); err == nil {
		t.Fatal("prepareAppSensitiveFile accepted a symlink")
	}
}

func assertAppMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
