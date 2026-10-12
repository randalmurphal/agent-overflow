package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

// A backend killed with a Claude session live runs no Close, so the
// session's prompt files outlive it. They must be in the data directory,
// never in the shared temp directory, and the next boot on that data
// directory removes them.
func TestPromptFilesOfAKilledSessionAreRemovedAtTheNextBoot(t *testing.T) {
	sharedTemp := t.TempDir()
	t.Setenv("TMPDIR", sharedTemp)
	root := t.TempDir()
	dbDir := filepath.Join(root, "agent-overflow")
	dbPath := filepath.Join(dbDir, databaseFileName)
	migratedDatabaseAt(t, dbPath)
	app, _ := setupE2EAppOn(t, dbPath, dbDir)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{mockClaudeInitLine}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	live, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("no live session after StartSession")
	}
	promptDir := filepath.Join(dbDir, claudePromptDir)
	if files := promptFilesIn(t, promptDir); len(files) == 0 {
		t.Fatalf("no prompt file in %s for a session carrying the app guide", promptDir)
	}

	proc, err := os.FindProcess(live.ProviderSession().PID())
	if err != nil {
		t.Fatalf("find session process: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill session process: %v", err)
	}
	if files := promptFilesIn(t, sharedTemp); len(files) > 0 {
		t.Fatalf("prompt files in the shared temp directory: %v", files)
	}
	if files := promptFilesIn(t, promptDir); len(files) == 0 {
		t.Fatal("the killed session's prompt file is gone before any boot; the test no longer covers the leak")
	}

	next := NewApp()
	next.dataDirOverride = root
	_, st, err := next.initStores(context.Background())
	if err != nil {
		t.Fatalf("initStores: %v", err)
	}
	t.Cleanup(func() {
		if next.logger != nil {
			_ = next.logger.Close()
		}
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if _, err := os.Stat(promptDir); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%s) = %v after the next boot, want removed", promptDir, err)
	}
}

// A session cannot start without a data directory for its prompt files:
// the app refuses rather than writing them to the shared temp directory.
func TestClaudePromptFileDirRequiresTheDataDirectory(t *testing.T) {
	t.Parallel()
	if dir, err := (&App{}).claudePromptFileDir(); err == nil {
		t.Fatalf("claudePromptFileDir() = %q with no data directory, want an error", dir)
	}
	dataDir := t.TempDir()
	dir, err := (&App{configDir: dataDir}).claudePromptFileDir()
	if err != nil {
		t.Fatalf("claudePromptFileDir: %v", err)
	}
	if want := filepath.Join(dataDir, claudePromptDir); dir != want {
		t.Fatalf("claudePromptFileDir() = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat prompt dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("prompt dir mode = %o, want 0700", perm)
	}
}

func promptFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "ao-claude-system-prompt-*.txt"))
	if err != nil {
		t.Fatalf("glob prompt files: %v", err)
	}
	return matches
}
