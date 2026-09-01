package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"agent-overflow/internal/diagenv"
	"agent-overflow/internal/harnessrpc"
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

func TestMockControlEnvironmentIsInstalledBeforeAppStart(t *testing.T) {
	for _, path := range []string{"main_harness.go", "main_soak.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(source)
		startControl := strings.Index(text, "harnessrpc.StartControl(h)")
		installEnv := strings.Index(text, "appservice.SetProviderExtraEnv(appService.App, providerEnv)")
		startApp := strings.Index(text, "appService.Start(bootCtx)")
		if startControl < 0 || installEnv < 0 || startApp < 0 || !(startControl < installEnv && installEnv < startApp) {
			t.Fatalf(
				"%s boot order changed: StartControl=%d provider env=%d App.Start=%d; mock credentials must be installed before startup",
				path, startControl, installEnv, startApp,
			)
		}
	}
}

// TestRealBrowserEngineIsOptInAndNeverArmedForTheSoak covers the whole
// pin decision (docs/specs/embedded-browser.md §10). Two properties, and
// the default-off one is the safety property: every unattended run —
// `make go-test`, `make e2e`, a Playwright leg — must keep the fake
// engine, because the only failure mode of getting this wrong is a
// browser silently launched on a machine nobody is watching.
func TestRealBrowserEngineIsOptInAndNeverArmedForTheSoak(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		flags cliFlags
		want  bool
	}{
		{"harness, no opt-in", "", cliFlags{harness: true}, false},
		{"harness, opt-in", "1", cliFlags{harness: true}, true},
		{"harness, opt-in spelled true", "TRUE", cliFlags{harness: true}, true},
		{"harness, non-truthy value", "0", cliFlags{harness: true}, false},
		{"harness, garbage value", "maybe", cliFlags{harness: true}, false},
		{"windowed harness, opt-in", "1", cliFlags{harness: true, window: true}, true},
		// The Windows harness (`make harness-wsl`) rides the launcher-owned
		// --soak wire flag WITHOUT --autopilot. It is attended, so it lifts.
		{"launcher-shell harness, opt-in", "1", cliFlags{soak: true}, true},
		{"launcher-shell harness, no opt-in", "", cliFlags{soak: true}, false},
		// --autopilot is what makes an isolated instance a SOAK. Unattended
		// for hours: the pin is unconditional there, opt-in or not.
		{"soak autopilot, opt-in", "1", cliFlags{soak: true, autopilot: true}, false},
		{"soak autopilot, no opt-in", "", cliFlags{soak: true, autopilot: true}, false},
		{"windowed soak autopilot, opt-in", "1", cliFlags{soak: true, autopilot: true, window: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(diagenv.HarnessRealBrowser, tc.env)
			if got := realBrowserEngineRequested(tc.flags); got != tc.want {
				t.Fatalf("realBrowserEngineRequested(%+v) with %s=%q = %v, want %v",
					tc.flags, diagenv.HarnessRealBrowser, tc.env, got, tc.want)
			}
		})
	}
}

// The pin is the NEGATION of the opt-in, so the zero isolationOptions —
// what a caller that forgot to fill it in would pass — must still pin the
// fake engine.
func TestZeroIsolationOptionsKeepTheFakeBrowserEngine(t *testing.T) {
	if (isolationOptions{}).RealBrowserEngine {
		t.Fatal("zero isolationOptions requests a real browser engine; the safe default must be the zero value")
	}
}

// The opt-in has to reach the WSL backend, which runs on the far side of
// two WSLENV hops (`make harness-wsl`). diagenv.Passthrough is hop 2, the
// one the launcher owns; DEV_WSL_FWD_VARS is hop 1, from the WSL shell to
// the Windows launcher. Missing either leaves the variable set in a
// terminal that the backend never hears about.
func TestRealBrowserOptInCrossesTheWSLBoundary(t *testing.T) {
	found := false
	for _, name := range diagenv.Passthrough() {
		if name == diagenv.HarnessRealBrowser {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not in diagenv.Passthrough(); the Windows harness backend would never see it", diagenv.HarnessRealBrowser)
	}
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, line := range strings.Split(string(makefile), "\n") {
		if !strings.HasPrefix(line, "DEV_WSL_FWD_VARS :=") {
			continue
		}
		if !strings.Contains(line, diagenv.HarnessRealBrowser) {
			t.Fatalf("DEV_WSL_FWD_VARS omits %s; the WSL shell would never hand it to the Windows launcher", diagenv.HarnessRealBrowser)
		}
		return
	}
	t.Fatal("DEV_WSL_FWD_VARS not found in the Makefile")
}

// TestBrowserWindowGetterIsInstalledBeforeAppStart is the sibling of
// TestMockControlEnvironmentIsInstalledBeforeAppStart, for the other
// write-once boot input.
//
// SetBrowserNativeWindow must run before App.Start: the browser Manager is
// built during startup and selects its engine from whether a getter
// EXISTS. An isolated boot starts its backend long before any window,
// which works only because the pointer behind the getter is resolved
// lazily — so the getter is installed (still empty) inside
// newIsolatedProviderApp, and this ordering is what keeps that call on the
// right side of Start.
func TestBrowserWindowGetterIsInstalledBeforeAppStart(t *testing.T) {
	for _, path := range []string{"main_harness.go", "main_soak.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(source)
		build := strings.Index(text, "newIsolatedProviderApp(paths, isolationOptions{")
		startApp := strings.Index(text, "appService.Start(bootCtx)")
		if build < 0 || startApp < 0 || build > startApp {
			t.Fatalf("%s: newIsolatedProviderApp=%d App.Start=%d; the browser window getter must be installed before startup", path, build, startApp)
		}
	}
	// The install itself lives in that one helper, so a third isolated boot
	// mode cannot pick up the pins without also picking up the window.
	source, err := os.ReadFile("main_harness.go")
	if err != nil {
		t.Fatalf("read main_harness.go: %v", err)
	}
	if strings.Count(string(source), "appservice.SetBrowserNativeWindow(") != 1 {
		t.Fatal("main_harness.go must install the browser window getter exactly once, inside newIsolatedProviderApp")
	}
}

// The window cell is written by the Wails app loop and read by the browser
// engine's start path, so it answers nil safely before the shell publishes
// and returns the live handle after.
func TestIsolatedNativeWindowAnswersNilUntilPublished(t *testing.T) {
	window := &isolatedNativeWindow{}
	if window.pointer() != nil {
		t.Fatal("an unpublished window cell returned a pointer")
	}
	var handle int
	wantState := harnessrpc.WindowState{Bounds: harnessrpc.WindowRect{Width: 1100, Height: 720}}
	var gotCommand harnessrpc.WindowCommand
	window.publish(
		func() unsafe.Pointer { return unsafe.Pointer(&handle) },
		func() (harnessrpc.WindowState, error) { return wantState, nil },
		func(command harnessrpc.WindowCommand) error { gotCommand = command; return nil },
	)
	if window.pointer() != unsafe.Pointer(&handle) {
		t.Fatal("the published window handle did not reach the getter")
	}
	if got, err := window.State(); err != nil || got != wantState {
		t.Fatalf("published window state = %+v, %v; want %+v", got, err, wantState)
	}
	wantCommand := harnessrpc.WindowCommand{Action: "maximize"}
	if err := window.Command(wantCommand); err != nil || gotCommand != wantCommand {
		t.Fatalf("published window command = %+v, %v; want %+v", gotCommand, err, wantCommand)
	}
	// A windowed boot whose window has not materialized yet answers nil
	// through the published getter, which is the ordinary case between
	// app.Run and ApplicationStarted.
	window.publish(func() unsafe.Pointer { return nil }, nil, nil)
	if window.pointer() != nil {
		t.Fatal("a published getter answering nil did not surface as nil")
	}
}
