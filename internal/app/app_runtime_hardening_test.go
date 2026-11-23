package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	migratedDatabaseAt(t, dbPath)
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatalf("loosen db file: %v", err)
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
	gotDir, st, err := app.initStores(context.Background())
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

// initStores' boot acts on the credential slots under the user's HOME while
// its metadata store lives under the data dir. When those roots disagree (a
// fresh --data-dir, a test overriding the config root but not the home) the
// metadata names none of the home's slots (incident 2026-07-29). A boot over
// empty metadata must leave existing slots untouched.
func TestInitStoresKeepsCredentialSlotsWhenMetadataIsEmpty(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	seeded := newTestProviderCredentials(t, homeDir)
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
	migratedDatabaseAt(t, filepath.Join(app.dataDirOverride, "agent-overflow", databaseFileName))
	_, st, err := app.initStores(context.Background())
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
	t.Parallel()
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

// The account boot steps of a data root whose store already lists an account
// of its own remove the slot of its own unfinished sign-in and keep a slot of
// another data root on the same home. A slot that cannot be removed is
// reported and does not stop the steps that follow.
func TestBootStepsRemoveOnlyThisRootsUnfinishedSignIns(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	dataRoot := t.TempDir()
	dbDir := filepath.Join(dataRoot, "agent-overflow")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, homeDir)
	codexHome := filepath.Join(homeDir, ".codex")
	claudeHome := filepath.Join(homeDir, ".claude")
	seeded, err := provideraccounts.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seeded.UpsertAndActivate(provideraccounts.Account{ID: "own-account", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	unfinished, err := seeded.ReserveAccountID("codex", codexHome)
	if err != nil {
		t.Fatal(err)
	}
	stuck, err := seeded.ReserveAccountID("claude", claudeHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"own-account", unfinished, "another-roots-login"} {
		if err := credentials.WriteAccountCredential("codex", id, []byte(`{"account":"`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// A regular file where the stuck slot's directory belongs: it can be
	// neither read nor removed.
	if err := os.MkdirAll(filepath.Join(claudeHome, "agent-overflow-accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, "agent-overflow-accounts", stuck), []byte("not a slot"), 0o600); err != nil {
		t.Fatal(err)
	}

	migratedDatabaseAt(t, filepath.Join(dbDir, databaseFileName))
	app := NewApp()
	app.dataDirOverride = dataRoot
	progress := newRecordingBootProgress("")
	SetBootProgress(app, progress)
	_, st, err := app.initStores(context.Background())
	if err != nil {
		t.Fatalf("initStores: %v", err)
	}
	defer st.Close()
	if app.logger != nil {
		defer app.logger.Close()
	}
	for i, step := range app.providerAccountBootSteps() {
		if err := step(); err != nil {
			t.Fatalf("account boot step %d failed the boot: %v", i, err)
		}
	}

	for _, id := range []string{"own-account", "another-roots-login"} {
		if _, err := credentials.ReadCredential("codex", id, false); err != nil {
			t.Fatalf("the boot deleted codex slot %s: %v", id, err)
		}
	}
	if _, err := credentials.ReadCredential("codex", unfinished, false); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("the unfinished sign-in's slot survived the boot (err %v)", err)
	}
	failures := progress.failures()
	if len(failures) != 1 || !strings.Contains(failures[0], stuck) {
		t.Fatalf("reported boot failures = %q, want one naming the claude slot %s", failures, stuck)
	}
}
