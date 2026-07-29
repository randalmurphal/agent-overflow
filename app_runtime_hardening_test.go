package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agent-overflow/internal/provideraccounts"
)

func TestInitStoresRepairsAppOwnedPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	configRoot := t.TempDir()
	// initStores builds the provider credential store from os.UserHomeDir(),
	// and its startup prune deletes credential slots. Without this redirect
	// the test sweeps the DEVELOPER'S real ~/.claude and ~/.codex saved
	// logins (incident 2026-07-29: every `make go-test` silently destroyed
	// the machine's saved provider accounts).
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
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
	app.dataDirOverride = configRoot
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

// initStores' startup prune sweeps the credential slots under the user's
// HOME while its keep-set comes from the metadata store under the data dir.
// When those roots disagree — a fresh --data-dir, a test overriding the
// config root but not the home — the metadata lists no accounts, and before
// the empty-keep-set guard the prune then deleted every saved login on the
// machine (incident 2026-07-29). A boot over empty metadata must leave
// existing slots untouched.
func TestInitStoresKeepsCredentialSlotsWhenMetadataIsEmpty(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	seeded, err := provideraccounts.NewCredentials(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, providerName := range []string{"claude", "codex"} {
		if err := seeded.WriteAccountCredential(
			providerName,
			"saved-login",
			[]byte(`{"account":"precious"}`),
		); err != nil {
			t.Fatal(err)
		}
	}

	app := NewApp()
	app.dataDirOverride = t.TempDir()
	_, st, err := app.initStores()
	if err != nil {
		t.Fatalf("initStores: %v", err)
	}
	defer st.Close()
	if app.logger != nil {
		defer app.logger.Close()
	}

	for _, providerName := range []string{"claude", "codex"} {
		if _, err := seeded.ReadCredential(providerName, "saved-login", false); err != nil {
			t.Fatalf("%s slot destroyed by a boot with empty metadata: %v", providerName, err)
		}
	}
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
