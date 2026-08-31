package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

func TestParseFlagsSoakDefaultsAnIsolatedDataDir(t *testing.T) {
	flags, err := parseFlags([]string{"--soak", "--autopilot", "--listen", "127.0.0.1:0", "--print-url-fd", "0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flags.soak {
		t.Fatal("--soak did not select the launcher-shell isolated backend")
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
		// Both flags are meaningless outside the isolated backend, and a
		// stray --launcher-pid would publish a pid `ao-harness down` might
		// later signal.
		{"--autopilot"},
		{"--harness", "--data-dir", "/tmp/x", "--autopilot"},
		{"--launcher-pid", "4242"},
		{"--soak", "--launcher-pid", "-4242"},
		{"--isolated-profile", "perf"},
		{"--soak", "--isolated-profile", "unknown"},
		{"--soak", "--isolated-profile", "perf", "--autopilot"},
	}
	for _, args := range cases {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v) accepted conflicting flags, want error", args)
		}
	}
}

func TestParseFlagsPerfUsesItsOwnRootAndIdentity(t *testing.T) {
	flags, err := parseFlags([]string{"--soak", "--isolated-profile", "PERF", "--print-url-fd", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if flags.dataDir != perfDefaultDataRoot() {
		t.Fatalf("perf data dir = %q, want %q", flags.dataDir, perfDefaultDataRoot())
	}
	if flags.dataDir == harnessDefaultDataRoot() || flags.dataDir == soakDefaultDataRoot() {
		t.Fatalf("perf data dir collides with another launcher profile: %q", flags.dataDir)
	}
	if got := isolatedBootMode(flags); got != instanceinfo.ModePerf {
		t.Fatalf("perf mode = %q, want %q", got, instanceinfo.ModePerf)
	}
}

// The rename's central rule: --soak is the isolated launcher-shell
// BACKEND, and --autopilot is the soak preset on top of it. A --soak boot
// with no autopilot is the Windows harness — different default data root,
// different stamped mode — and must never seed threads or start a turn.
func TestParseFlagsSoakWithoutAutopilotIsTheHarnessInstance(t *testing.T) {
	flags, err := parseFlags([]string{"--soak", "--print-url-fd", "0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flags.autopilot {
		t.Fatal("--soak armed the autopilot on its own; the soak preset must be explicit")
	}
	if got, want := flags.dataDir, harnessDefaultDataRoot(); got != want {
		t.Fatalf("data dir = %q, want %q", got, want)
	}
	if got := isolatedBootMode(flags); got != instanceinfo.ModeHarness {
		t.Fatalf("mode = %q, want %q", got, instanceinfo.ModeHarness)
	}

	soak, err := parseFlags([]string{"--soak", "--autopilot", "--print-url-fd", "0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got, want := soak.dataDir, soakDefaultDataRoot(); got != want {
		t.Fatalf("soak data dir = %q, want %q", got, want)
	}
	if got := isolatedBootMode(soak); got != instanceinfo.ModeSoak {
		t.Fatalf("mode = %q, want %q", got, instanceinfo.ModeSoak)
	}
	// The two roots cannot be one directory: the autopilot refuses a data
	// dir holding threads it did not seed, so a shared root would make
	// whichever booted second fail to arm.
	if flags.dataDir == soak.dataDir {
		t.Fatalf("harness and soak share the data root %q", flags.dataDir)
	}
}

// The launcher pid crosses the WSL boundary as an explicit arg and is
// published for a teardown to signal, so it has to survive parsing
// exactly.
func TestParseFlagsCarriesTheLauncherPID(t *testing.T) {
	flags, err := parseFlags([]string{
		"--soak",
		"--launcher-pid", "4242",
		"--launcher-start-time", "133485408000000000",
		"--launcher-executable", `C:\Agent Overflow\agent-overflow.exe`,
		"--launcher-profile", "harness",
		"--launcher-webview-profile", `C:\Agent Overflow\webview2-harness`,
		"--print-url-fd", "0",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flags.launcherPID != 4242 {
		t.Fatalf("launcherPID = %d, want 4242", flags.launcherPID)
	}
	if flags.launcherStartTime != "133485408000000000" || flags.launcherProfile != "harness" {
		t.Fatalf("launcher identity was not preserved: %#v", flags)
	}
	bare, err := parseFlags([]string{"--soak", "--print-url-fd", "0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if bare.launcherPID != 0 {
		t.Fatalf("launcherPID = %d with no launcher, want 0", bare.launcherPID)
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
// The first four pins below are what make a mocked boot mode incapable of
// reaching a real provider binary or the developer's real provider
// homes. They are applied in exactly ONE function
// (app.ConfigureIsolation through newIsolatedProviderApp), so a future mode — a second soak variant, a
// profiling boot, whatever — either calls that helper and gets all four,
// or fails this test. Three of four is the failure shape that burned a
// real login (see the incident history in CLAUDE.md).
//
// mockEngine rides the same rule for the same reason. Unlike the four it
// is LIFTABLE (realBrowserEngineRequested, the manual real-engine gate in
// docs/specs/embedded-browser.md §10), which makes a second assignment
// site even worse: the lift would then have two places to disagree about,
// and the failure is a browser silently launched on an unattended rig.
// One assignment site keeps "default-on, lifted in exactly one function"
// checkable.
func TestMockedBootModesShareOneIsolationHelper(t *testing.T) {
	pins := []string{
		"providerBinaryOverride",
		"fileKeychainOverride",
		"credentialHomeOverride",
		"backgroundFetchDisabled",
		"mockEngine",
	}
	assignment := make([]*regexp.Regexp, len(pins))
	for i, pin := range pins {
		assignment[i] = regexp.MustCompile(`\.` + pin + `\s*=[^=]`)
	}

	found := map[string][]string{}
	for _, dir := range []string{".", "internal/app"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for i, pin := range pins {
				if assignment[i].Match(src) {
					found[pin] = append(found[pin], path)
				}
			}
		}
	}

	for _, pin := range pins {
		files := found[pin]
		if len(files) != 1 || files[0] != filepath.Join("internal", "app", "bootstrap.go") {
			t.Errorf(
				"%s is assigned in %v; it must be set only by app.ConfigureIsolation "+
					"so every mocked boot mode (--harness, --soak, ...) gets the complete isolation set",
				pin, files)
		}
	}
}
