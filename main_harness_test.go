package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFlagsHarnessRequiresDataDir(t *testing.T) {
	_, err := parseFlags([]string{"--harness"})
	if err == nil {
		t.Fatal("parseFlags accepted --harness without --data-dir, want error")
	}
	if !strings.Contains(err.Error(), "--data-dir") {
		t.Fatalf("error %q does not mention --data-dir", err)
	}
}

func TestParseFlagsHarnessConflicts(t *testing.T) {
	cases := [][]string{
		{"--harness", "--data-dir", "/tmp/x", "--connect", "ws://host:1?token=t"},
		{"--harness", "--data-dir", "/tmp/x", "--print-url-fd", "3"},
		{"--data-dir", "/tmp/x", "--connect", "ws://host:1?token=t"},
		{"--mock-provider", "/tmp/mp"},
	}
	for _, args := range cases {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v) accepted conflicting flags, want error", args)
		}
	}
}

func TestParseFlagsHarnessAccepted(t *testing.T) {
	flags, err := parseFlags([]string{"--harness", "--data-dir", "/tmp/x", "--listen", "127.0.0.1:0", "--mock-provider", "/tmp/mp"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flags.harness || flags.dataDir != "/tmp/x" || flags.mockProvider != "/tmp/mp" {
		t.Fatalf("flags = %+v, want harness with data dir and mock provider", flags)
	}
	if flags.headless {
		t.Fatal("--harness must not set the --print-url-fd headless mode; it has its own boot path")
	}
}

func TestShouldSyncShellEnvSkipsHarness(t *testing.T) {
	if shouldSyncShellEnv(cliFlags{harness: true, dataDir: "/tmp/x"}) {
		t.Fatal("harness mode should skip shellenv sync")
	}
}

func TestRefuseRealDataDir(t *testing.T) {
	defaultRoot, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this host: %v", err)
	}
	if err := refuseRealDataDir(defaultRoot); err == nil {
		t.Fatal("accepted the OS config root as --data-dir")
	}
	if err := refuseRealDataDir(filepath.Join(defaultRoot, "agent-overflow")); err == nil {
		t.Fatal("accepted the real app data dir as --data-dir")
	}
	if err := refuseRealDataDir(t.TempDir()); err != nil {
		t.Fatalf("rejected a scratch dir: %v", err)
	}
}

func TestPrepareHarnessRefusesSymlinkedPaths(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	// The data root itself is a link — every child path would resolve
	// through it (the `make harness` default /tmp/agent-overflow-harness
	// is a predictable plant target).
	rootLink := filepath.Join(base, "root-link")
	if err := os.Symlink(target, rootLink); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if _, err := prepareHarness(cliFlags{dataDir: rootLink}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("prepareHarness with symlinked data root: err = %v, want symlink refusal", err)
	}

	// The agent-overflow child under an honest root is a link.
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "agent-overflow")); err != nil {
		t.Fatalf("symlink data dir: %v", err)
	}
	if _, err := prepareHarness(cliFlags{dataDir: root}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("prepareHarness with symlinked data dir: err = %v, want symlink refusal", err)
	}
}

func TestIsolateHarnessHome(t *testing.T) {
	root := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	home, err := isolateHarnessHome(root)
	if err != nil {
		t.Fatalf("isolateHarnessHome: %v", err)
	}
	if home != filepath.Join(root, "home") {
		t.Fatalf("home = %q, want %q", home, filepath.Join(root, "home"))
	}
	if os.Getenv("HOME") != home {
		t.Fatalf("HOME = %q, want %q", os.Getenv("HOME"), home)
	}
	gitconfig, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatalf("read seeded .gitconfig: %v", err)
	}
	if !strings.Contains(string(gitconfig), "harness@agent-overflow.invalid") {
		t.Fatalf(".gitconfig missing harness identity: %q", gitconfig)
	}

	// A pre-existing .gitconfig (agent customised it between runs) must
	// survive a second boot.
	custom := "[user]\n\tname = Custom\n\temail = c@example.com\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(custom), 0o600); err != nil {
		t.Fatalf("write custom .gitconfig: %v", err)
	}
	if _, err := isolateHarnessHome(root); err != nil {
		t.Fatalf("second isolateHarnessHome: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatalf("re-read .gitconfig: %v", err)
	}
	if string(after) != custom {
		t.Fatal("second boot clobbered a customised .gitconfig")
	}
}

func TestIsolateHarnessHomeKeepHomeOptOut(t *testing.T) {
	t.Setenv(harnessKeepHomeEnv, "1")
	origHome := os.Getenv("HOME")

	home, err := isolateHarnessHome(t.TempDir())
	if err != nil {
		t.Fatalf("isolateHarnessHome: %v", err)
	}
	if home != "" {
		t.Fatalf("home = %q, want empty (opt-out)", home)
	}
	if os.Getenv("HOME") != origHome {
		t.Fatal("opt-out must leave HOME untouched")
	}
}

func TestResolveMockProviderValidatesExistence(t *testing.T) {
	if _, err := resolveMockProvider(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted a missing mock provider binary")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "mp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	got, err := resolveMockProvider(bin)
	if err != nil {
		t.Fatalf("resolveMockProvider: %v", err)
	}
	if got != bin {
		t.Fatalf("resolved %q, want %q", got, bin)
	}
}

func TestSeedHarnessSettings(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "ao-mockprovider")
	if err := seedHarnessSettings(dir, mp); err != nil {
		t.Fatalf("seedHarnessSettings: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read seeded settings: %v", err)
	}
	var got struct {
		ClaudeBinaryPath             string `json:"claudeBinaryPath"`
		CodexBinaryPath              string `json:"codexBinaryPath"`
		ObservabilityEventLogEnabled bool   `json:"observabilityEventLogEnabled"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse seeded settings: %v", err)
	}
	if got.ClaudeBinaryPath != mp || got.CodexBinaryPath != mp {
		t.Fatalf("binary paths = (%q, %q), want both %q", got.ClaudeBinaryPath, got.CodexBinaryPath, mp)
	}
	if !got.ObservabilityEventLogEnabled {
		t.Fatal("event log must be enabled for harness recordings")
	}
}

func TestHarnessBootstrapPrefixIsStable(t *testing.T) {
	// e2e/lib/harness.ts and any agent script parsing harness output key
	// off this literal. Changing it is a cross-repo breaking change.
	if harnessStdoutPrefix != "__AO_HARNESS__:" {
		t.Fatalf("harnessStdoutPrefix = %q; update every parser before changing it", harnessStdoutPrefix)
	}
}
