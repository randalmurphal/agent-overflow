package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

// Windowed-mode wave W1 (docs/specs/testing-harness.md §1-§2): the flag
// combinations, the per-worktree data root, the XDG derivation, the
// window title, and the shape of the data-dir instance file. Everything
// here is pure or filesystem-only — no App is constructed, so no test in
// this file can reach a provider spawn path.

func TestParseFlagsWindowRequiresAnIsolatedMode(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bare", []string{"--window"}},
		{"with data dir only", []string{"--window", "--data-dir", "/tmp/x"}},
		{"with connect", []string{"--window", "--connect", "ws://host:1?token=t"}},
		{"harness with print-url-fd", []string{"--window", "--harness", "--data-dir", "/tmp/x", "--print-url-fd", "3"}},
		{"soak with print-url-fd", []string{"--window", "--soak", "--print-url-fd", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseFlags(tc.args); err == nil {
				t.Fatalf("parseFlags(%v) accepted the combination, want an error", tc.args)
			}
		})
	}
}

func TestParseFlagsWindowWithHarnessAndSoak(t *testing.T) {
	harness, err := parseFlags([]string{"--harness", "--window", "--data-dir", "/tmp/w1"})
	if err != nil {
		t.Fatalf("parseFlags(--harness --window): %v", err)
	}
	if !harness.window || !harness.harness {
		t.Fatalf("flags = %+v, want harness+window", harness)
	}
	if harness.headless {
		t.Fatal("--window must not select the headless bootstrap channel")
	}

	soak, err := parseFlags([]string{"--soak", "--window"})
	if err != nil {
		t.Fatalf("parseFlags(--soak --window): %v", err)
	}
	if !soak.window || !soak.soak {
		t.Fatalf("flags = %+v, want soak+window", soak)
	}
}

func TestParseFlagsSoakWindowDefaultsToThePerWorktreeRoot(t *testing.T) {
	// A native windowed soak is started by hand, once per checkout, so it
	// must not land on the single launcher-owned ~/.agent-overflow-soak
	// that a second worktree would also claim.
	flags, err := parseFlags([]string{"--soak", "--window"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if want := instanceinfo.DefaultSoakDataRoot(); flags.dataDir != want {
		t.Fatalf("dataDir = %q, want the per-worktree soak root %q", flags.dataDir, want)
	}
	if flags.dataDir == soakDefaultDataRoot() {
		t.Fatal("windowed soak fell back to the launcher's shared data root")
	}
	if flags.dataDir == instanceinfo.DefaultDataRoot() {
		// The -soak suffix keeps a windowed soak off the checkout's
		// harness root: the soak autopilot refuses a data dir holding
		// threads it did not seed, so sharing one root would fail the
		// second boot.
		t.Fatal("windowed soak shares the harness per-worktree root")
	}
	if err := refuseRealDataDir(flags.dataDir); err != nil {
		t.Fatalf("per-worktree soak root is not isolated: %v", err)
	}
}

func TestParseFlagsLauncherSoakKeepsItsDefaultRoot(t *testing.T) {
	// The launcher spells `--soak --print-url-fd 0` and nothing else;
	// that path must keep pointing at the well-known root make soak-check
	// reads.
	flags, err := parseFlags([]string{"--soak", "--print-url-fd", "0", "--listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flags.dataDir != soakDefaultDataRoot() {
		t.Fatalf("dataDir = %q, want %q", flags.dataDir, soakDefaultDataRoot())
	}
}

func TestWebviewStorageEnvLivesUnderTheDataRoot(t *testing.T) {
	root := filepath.FromSlash("/tmp/w1-root")
	vars := webviewStorageEnv(root)
	if len(vars) != 3 {
		t.Fatalf("webviewStorageEnv returned %d vars, want 3", len(vars))
	}
	want := map[string]string{
		"XDG_CACHE_HOME":  filepath.Join(root, "home", "xdg", "cache"),
		"XDG_CONFIG_HOME": filepath.Join(root, "home", "xdg", "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "home", "xdg", "data"),
	}
	seen := map[string]bool{}
	for _, v := range vars {
		if seen[v.Name] {
			t.Errorf("%s derived twice", v.Name)
		}
		seen[v.Name] = true
		if want[v.Name] != v.Dir {
			t.Errorf("%s = %q, want %q", v.Name, v.Dir, want[v.Name])
		}
		if !strings.HasPrefix(v.Dir, root) {
			t.Errorf("%s = %q escapes the data root %q", v.Name, v.Dir, root)
		}
	}
	// Stable order so two boot logs are diffable.
	if vars[0].Name != "XDG_CACHE_HOME" || vars[2].Name != "XDG_DATA_HOME" {
		t.Errorf("webviewStorageEnv order changed: %+v", vars)
	}
}

func TestIsolatedWindowTitleNamesModeAndInstance(t *testing.T) {
	harness := isolatedWindowTitle(instanceinfo.ModeHarness, "0f3a91cc")
	if harness != "Agent Overflow (harness · 0f3a91cc)" {
		t.Fatalf("harness title = %q", harness)
	}
	soak := isolatedWindowTitle(instanceinfo.ModeSoak, "deadbeef")
	if soak != "Agent Overflow (soak · deadbeef)" {
		t.Fatalf("soak title = %q", soak)
	}
	if harness == soak {
		t.Fatal("two instances share one window title")
	}
}

func TestHarnessInstanceFileCarriesBootstrapAndIdentity(t *testing.T) {
	// The data-dir file is what lets a tool attach to a RUNNING instance
	// without its stdout, so it must carry the whole bootstrap payload
	// (token included) plus the identity that ties it to a registry row.
	file := harnessInstanceFile{
		harnessBootstrap: harnessBootstrap{
			URL:          "http://127.0.0.1:4321/?token=t",
			Port:         4321,
			Token:        "t",
			DataRoot:     "/tmp/root",
			DataDir:      "/tmp/root/agent-overflow",
			MockProvider: "/tmp/bin/ao-mockprovider",
			PID:          99,
			Version:      "dev",
		},
		Identity: instanceinfo.Identity{
			ID:        "0f3a91cc",
			Mode:      instanceinfo.ModeHarness,
			Window:    true,
			Worktree:  "/repo",
			StartedAt: "2026-08-26T00:00:00Z",
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"url", "port", "token", "dataRoot", "dataDir", "mockProvider", "pid", "version",
		"id", "mode", "window", "worktree", "startedAt",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("instance file is missing %q: %s", key, data)
		}
	}
}

func TestPublishedInstanceRemoveIsIdempotent(t *testing.T) {
	// Both the shutdown path and the window-failure path call remove();
	// the second call must be a no-op rather than a logged error storm.
	dir := t.TempDir()
	registry := t.TempDir()
	path := filepath.Join(dir, instanceinfo.InstanceFileName)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed instance file: %v", err)
	}
	row := filepath.Join(registry, "0f3a91cc.json")
	if err := os.WriteFile(row, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed registry row: %v", err)
	}
	published := publishedInstance{id: "0f3a91cc", filePath: path, registryDir: registry}
	published.remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("instance file survived removal: %v", err)
	}
	if _, err := os.Stat(row); !os.IsNotExist(err) {
		t.Fatalf("registry row survived removal: %v", err)
	}
	published.remove()

	// The zero value belongs to a boot that never published (startup
	// failure) and must remove nothing.
	publishedInstance{}.remove()
}

func TestHarnessRegistryDirEscapesTheWebviewStorageRedirect(t *testing.T) {
	// The registry only works as discovery if it is in the USER's cache
	// dir. isolateWebviewStorage repoints XDG_CACHE_HOME at the data
	// root, and os.UserCacheDir reads that variable, so the directory has
	// to be resolved before the redirect — which is why it is a package
	// var, initialized before main runs. This test moves the variable the
	// way a windowed boot does and proves the captured value did not
	// follow it (the bug this ordering exists to prevent, seen live
	// 2026-08-26).
	if harnessRegistryDir == "" {
		t.Skip("no user cache dir on this host")
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "isolated"))
	redirected, err := instanceinfo.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	if harnessRegistryDir == redirected {
		t.Fatalf("registry dir followed XDG_CACHE_HOME to %q; discovery would be invisible", redirected)
	}
	if !strings.HasSuffix(harnessRegistryDir, filepath.Join("agent-overflow", "harness-instances")) {
		t.Errorf("registry dir = %q, want the agent-overflow/harness-instances leaf", harnessRegistryDir)
	}
}

// TestIsolatedBootModesConstructTheirAppThroughOneHelper is the
// windowed-mode companion to TestMockedBootModesShareOneIsolationHelper
// (main_soak_test.go). That test proves the four provider pins are
// assigned in one place; this one proves the boot modes that need them
// still go THROUGH that place. A windowed variant that reached for
// newApp() directly would compile, boot, open a window, and be able to
// spawn the developer's real claude binary — with every pin still
// correctly assigned in main_harness.go.
func TestIsolatedBootModesConstructTheirAppThroughOneHelper(t *testing.T) {
	sources := repoRootGoSources(t)

	callers := map[string]bool{}
	for name, src := range sources {
		// The definition line lives in main_harness.go and is not a call.
		body := strings.ReplaceAll(src, "func newIsolatedProviderApp(", "")
		if strings.Contains(body, "newIsolatedProviderApp(") {
			callers[name] = true
		}
	}
	for _, want := range []string{"main_harness.go", "main_soak.go"} {
		if !callers[want] {
			t.Errorf("%s no longer constructs its App through newIsolatedProviderApp", want)
		}
		delete(callers, want)
	}
	if len(callers) != 0 {
		t.Errorf("unexpected isolated-boot constructors in %v; a new mocked mode belongs in runHarness/runSoak", callers)
	}

	// The window shell is a shell: it must take the already-pinned App,
	// never build one.
	for _, name := range []string{"main_harness_window.go", "main_harness_instance.go"} {
		if strings.Contains(sources[name], "newApp()") {
			t.Errorf("%s constructs an App; the windowed path must reuse the isolated one runHarness/runSoak built", name)
		}
	}
}

// repoRootGoSources reads every non-test Go file in the repo root.
func repoRootGoSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(src)
	}
	return out
}
