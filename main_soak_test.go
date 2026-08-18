package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseFlagsSoakDefaultsAnIsolatedDataDir(t *testing.T) {
	flags, err := parseFlags([]string{"--soak", "--listen", "127.0.0.1:0", "--print-url-fd", "0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flags.soak {
		t.Fatal("--soak did not select soak mode")
	}
	if flags.dataDir == "" {
		t.Fatal("--soak left --data-dir empty; the Windows launcher cannot supply a Linux path")
	}
	if !filepath.IsAbs(flags.dataDir) {
		t.Fatalf("default soak data dir %q is not absolute", flags.dataDir)
	}
	// The default must be refused by nothing and shared with nothing: the
	// same check prepareHarness runs at boot.
	if err := refuseRealDataDir(flags.dataDir); err != nil {
		t.Fatalf("default soak data dir is not isolated: %v", err)
	}
	// --print-url-fd is the launcher's channel and must stay compatible.
	if !flags.headless {
		t.Fatal("--soak with --print-url-fd must keep the headless bootstrap channel")
	}
}

func TestParseFlagsSoakExplicitDataDirWins(t *testing.T) {
	flags, err := parseFlags([]string{"--soak", "--data-dir", "/tmp/soak-x", "--mock-provider", "/tmp/mp"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flags.dataDir != "/tmp/soak-x" {
		t.Fatalf("dataDir = %q, want the explicit value", flags.dataDir)
	}
	if flags.mockProvider != "/tmp/mp" {
		t.Fatalf("mockProvider = %q, want the explicit value", flags.mockProvider)
	}
}

func TestParseFlagsSoakConflicts(t *testing.T) {
	cases := [][]string{
		{"--soak", "--harness", "--data-dir", "/tmp/x"},
		{"--soak", "--connect", "ws://host:1?token=t"},
	}
	for _, args := range cases {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v) accepted conflicting flags, want error", args)
		}
	}
}

func TestShouldSyncShellEnvSkipsSoak(t *testing.T) {
	if shouldSyncShellEnv(cliFlags{soak: true, dataDir: "/tmp/x"}) {
		t.Fatal("soak mode should skip the login-shell PATH probe; it resolves no real tooling")
	}
}

func TestSoakScenarioIsShipped(t *testing.T) {
	if err := soakScenarioIsShipped(); err != nil {
		t.Fatalf("soak scenario %q: %v", soakScenarioName, err)
	}
}

// TestMockedBootModesShareOneIsolationHelper is the structural guard the
// permanent invariant in CLAUDE.md asks for: a boot mode that constructs
// an App able to start sessions must not hand-roll its own isolation.
//
// The four pins below are what make a mocked boot mode incapable of
// reaching a real provider binary or the developer's real provider
// homes. They are applied in exactly ONE function
// (newIsolatedProviderApp), so a future mode — a second soak variant, a
// profiling boot, whatever — either calls that helper and gets all four,
// or fails this test. Three of four is the failure shape that burned a
// real login (see the incident history in CLAUDE.md).
func TestMockedBootModesShareOneIsolationHelper(t *testing.T) {
	pins := []string{
		"providerBinaryOverride",
		"fileKeychainOverride",
		"credentialHomeOverride",
		"backgroundFetchDisabled",
	}
	assignment := make([]*regexp.Regexp, len(pins))
	for i, pin := range pins {
		assignment[i] = regexp.MustCompile(`\.` + pin + `\s*=[^=]`)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	found := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, pin := range pins {
			if assignment[i].Match(src) {
				found[pin] = append(found[pin], name)
			}
		}
	}

	for _, pin := range pins {
		files := found[pin]
		if len(files) != 1 || files[0] != "main_harness.go" {
			t.Errorf(
				"%s is assigned in %v; it must be set only by newIsolatedProviderApp in main_harness.go "+
					"so every mocked boot mode (--harness, --soak, ...) gets the complete isolation set",
				pin, files)
		}
	}
}
