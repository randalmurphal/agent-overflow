package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// spawnedClaudeArgv waits for the argv-recording mock Claude to run and
// returns what it was given, one value per line.
func spawnedClaudeArgv(t *testing.T, argvPath string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(argvPath)
		if err == nil && len(raw) > 0 {
			return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the spawned argv (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func argValue(argv []string, flag string) (string, bool) {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// TestStartSession_ClaudeCarriesTheAppGuideAndSnapshotOptOut asserts the
// Claude half end to end: the guide reaches the spawned argv as an append
// file whose content is the guide for this session's on servers, and the
// snapshot opt-out rides along once the installed version is known to
// accept it.
func TestStartSession_ClaudeCarriesTheAppGuideAndSnapshotOptOut(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := filepath.Join(t.TempDir(), "claude-argv.sh")
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + argvPath + "; done\ncat >/dev/null\n"
	mockexec.Write(t, binary, script)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	// What the startup probe or the watcher would have recorded for this
	// binary, keyed by its identity so the lookup trusts it.
	identity, ok := app.resolveProviderBinaryIdentity(string(provider.Claude))
	if !ok {
		t.Fatal("mock binary identity did not resolve")
	}
	app.providerBinaries.storeInstalled(string(provider.Claude), providerBinaryVersion{identity: identity, version: "2.1.284"})

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	appendPath, found := argValue(argv, "--append-system-prompt-file")
	if !found {
		t.Fatalf("argv has no --append-system-prompt-file: %v", argv)
	}
	content, err := os.ReadFile(appendPath)
	if err != nil {
		t.Fatalf("read the append file the session is running with: %v", err)
	}
	want := agentGuideText(map[string]bool{threadMCPName: true})
	if string(content) != want {
		t.Errorf("append file:\n%s\nwant:\n%s", content, want)
	}
	if snapshot, _ := argValue(argv, "--system-prompt-snapshot"); snapshot != "off" {
		t.Errorf("argv has no --system-prompt-snapshot off: %v", argv)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if _, err := os.Stat(appendPath); !os.IsNotExist(err) {
		t.Errorf("append file %s survived StopSession: %v", appendPath, err)
	}

	// Switched off: no append file, and the snapshot opt-out stays (it is
	// about the replacement override too).
	if err := os.Remove(argvPath); err != nil {
		t.Fatalf("reset argv: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": false}); err != nil {
		t.Fatalf("disable guide: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv = spawnedClaudeArgv(t, argvPath)
	if _, found := argValue(argv, "--append-system-prompt-file"); found {
		t.Errorf("guide off, yet argv carries an append file: %v", argv)
	}
	if snapshot, _ := argValue(argv, "--system-prompt-snapshot"); snapshot != "off" {
		t.Errorf("guide off must not drop the snapshot opt-out: %v", argv)
	}
}

// An unknown installed version omits the snapshot flag: an older build
// rejects it and the spawn would fail. The guide still rides.
func TestStartSession_ClaudeOmitsTheSnapshotFlagWithoutAKnownVersion(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := filepath.Join(t.TempDir(), "claude-argv.sh")
	mockexec.Write(t, binary, "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> "+argvPath+"; done\ncat >/dev/null\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	if _, found := argValue(argv, "--system-prompt-snapshot"); found {
		t.Errorf("version unknown, yet argv carries --system-prompt-snapshot: %v", argv)
	}
	if _, found := argValue(argv, "--append-system-prompt-file"); !found {
		t.Errorf("argv has no --append-system-prompt-file: %v", argv)
	}
}

// installedProviderVersion trusts a recorded version only while the file on
// disk is still the one that was probed.
func TestInstalledProviderVersionRequiresTheProbedBinary(t *testing.T) {
	app := newTestAppWithStore(t)
	if got := app.installedProviderVersion(string(provider.Claude)); got != "" {
		t.Fatalf("nothing recorded, yet version = %q", got)
	}
	binary := filepath.Join(t.TempDir(), "claude")
	writeProviderBinaryFile(t, binary, "exit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	stubProviderBinaryDetect(t, func(string) string { return "2.1.284 (Claude Code)" })
	app.sweepProviderBinaries()
	if got := app.installedProviderVersion(string(provider.Claude)); got != "2.1.284" {
		t.Fatalf("version after the baseline tick = %q, want 2.1.284", got)
	}
	writeProviderBinaryFile(t, binary, "exit 0 # upgraded since the last tick")
	if got := app.installedProviderVersion(string(provider.Claude)); got != "" {
		t.Fatalf("binary changed since the probe, yet version = %q", got)
	}
}

// The boot-time status probe seeds the same record the watcher keeps, so a
// session started in the first minute already knows its binary's version,
// and the watcher's first tick is a stat rather than a second probe.
func TestGetProviderStatusesSeedsTheInstalledVersionBaseline(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	claudeBinary := createMockBinary(t, "2.1.284 (Claude Code)")
	codexBinary := createMockBinary(t, "codex-cli 0.160.0")
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": claudeBinary,
		"codexBinaryPath":  codexBinary,
	}); err != nil {
		t.Fatalf("set binaries: %v", err)
	}
	if _, err := app.GetProviderStatuses(); err != nil {
		t.Fatalf("GetProviderStatuses: %v", err)
	}
	if got := app.installedProviderVersion(string(provider.Claude)); got != "2.1.284" {
		t.Errorf("claude version after the status probe = %q, want 2.1.284", got)
	}
	if got := app.installedProviderVersion(string(provider.Codex)); got != "0.160.0" {
		t.Errorf("codex version after the status probe = %q, want 0.160.0", got)
	}
}
