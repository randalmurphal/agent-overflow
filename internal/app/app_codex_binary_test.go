package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// codexNpmFixture is an npm global install of @openai/codex for this
// platform: bin/codex links to the package's bin/codex.js wrapper, and the
// platform package beside it vendors the native binary the wrapper spawns.
type codexNpmFixture struct {
	bin             string
	command         string
	packageRoot     string
	platformPackage string
	wrapper         string
	native          string
}

func newCodexNpmFixture(t *testing.T, prefix string) codexNpmFixture {
	t.Helper()
	var triple, platform string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		triple, platform = "x86_64-unknown-linux-musl", "codex-linux-x64"
	case "linux/arm64":
		triple, platform = "aarch64-unknown-linux-musl", "codex-linux-arm64"
	case "darwin/amd64":
		triple, platform = "x86_64-apple-darwin", "codex-darwin-x64"
	case "darwin/arm64":
		triple, platform = "aarch64-apple-darwin", "codex-darwin-arm64"
	default:
		t.Skipf("no npm Codex platform package for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	f := codexNpmFixture{
		bin:         filepath.Join(prefix, "bin"),
		packageRoot: filepath.Join(prefix, "lib", "node_modules", "@openai", "codex"),
	}
	f.command = filepath.Join(f.bin, "codex")
	f.wrapper = filepath.Join(f.packageRoot, "bin", "codex.js")
	f.platformPackage = filepath.Join(f.packageRoot, "node_modules", "@openai", platform)
	f.native = filepath.Join(f.platformPackage, "vendor", triple, "bin", "codex")
	for _, dir := range []string{f.bin, filepath.Dir(f.wrapper), filepath.Dir(f.native)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureFile(t, filepath.Join(f.packageRoot, "package.json"), `{"name":"@openai/codex","version":"0.162.1","bin":{"codex":"bin/codex.js"}}`)
	writeFixtureFile(t, filepath.Join(f.platformPackage, "package.json"), `{"name":"@openai/codex","version":"0.162.1-platform"}`)
	if _, err := os.Lstat(f.command); os.IsNotExist(err) {
		if err := os.Symlink("../lib/node_modules/@openai/codex/bin/codex.js", f.command); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// putCodexOnPath makes the fixture's bin directory the first PATH entry and
// leaves the default `codex` setting to production's exec.LookPath, so every
// lookup of the bare default, including the wrapper fallback, finds the
// fixture and never an installed Codex. Callers cannot be parallel.
func putCodexOnPath(t *testing.T, app *App, f codexNpmFixture) {
	t.Helper()
	t.Setenv("PATH", f.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	app.codexLookPathFn = nil
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": "codex"}); err != nil {
		t.Fatalf("restore default codex setting: %v", err)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// nativePath is where the resolver finds the fixture's native binary: under
// the wrapper's real directories, with the file itself unresolved, because
// a mock may be a link to the shared mockexec wrapper.
func nativePath(t *testing.T, f codexNpmFixture) string {
	t.Helper()
	return filepath.Join(realPath(t, filepath.Dir(f.native)), filepath.Base(f.native))
}

// The default setting runs the native binary the npm wrapper would spawn,
// so a Codex session is one process, and the spawn reaches the native
// binary rather than the wrapper on PATH.
func TestDefaultCodexSettingSpawnsTheNpmWrappersNativeBinary(t *testing.T) {
	app, bus := setupE2EApp(t)
	f := newCodexNpmFixture(t, t.TempDir())
	session := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:   `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-thread-native"}}}`,
		`"method":"config/read"`:  `{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}`,
	})
	script, err := os.ReadFile(session + ".payload")
	if err != nil {
		t.Fatalf("read mock session script: %v", err)
	}
	mockexec.Write(t, f.native, string(script))
	// The wrapper must never run: it refuses with a marker. A regular file,
	// as npm installs it, never a mockexec link.
	writeProviderBinaryFile(t, f.wrapper, "echo 'wrapper ran' >&2\nexit 97")
	putCodexOnPath(t, app, f)

	if got := app.providerBinaryPath(string(provider.Codex)); got != nativePath(t, f) {
		t.Fatalf("providerBinaryPath(codex) = %q, want the native binary %q", got, nativePath(t, f))
	}

	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
}

// Without the native binary the wrapper is the only thing that can run, and
// it stays the bare default so PATH lookup, spawn and errors are unchanged.
func TestDefaultCodexSettingKeepsTheWrapperWhenItsNativeBinaryIsMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	writeProviderBinaryFile(t, f.wrapper, "echo 'codex-cli 0.162.1'")
	putCodexOnPath(t, app, f)

	if got := app.providerBinaryPath(string(provider.Codex)); got != "codex" {
		t.Fatalf("providerBinaryPath(codex) = %q, want the bare default", got)
	}
	status := provider.DetectProvider(string(provider.Codex), app.providerBinaryPath(string(provider.Codex)))
	if status.BinaryPath != f.command || !status.Installed {
		t.Fatalf("DetectProvider = %+v, want the wrapper on PATH at %s", status, f.command)
	}
	if wrapper := app.codexWrapperFor(status.BinaryPath); wrapper != "" {
		t.Fatalf("codexWrapperFor(%q) = %q, want none for an unresolved wrapper", status.BinaryPath, wrapper)
	}
}

// An explicit setting is the user's choice of binary, even when it names the
// npm wrapper.
func TestExplicitCodexSettingIsRunAsGiven(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	writeProviderBinaryFile(t, f.native, "exit 0")
	app.codexLookPathFn = func(string) (string, error) { return f.command, nil }

	for _, explicit := range []string{f.command, f.wrapper} {
		if _, err := app.settings.Update(map[string]any{"codexBinaryPath": explicit}); err != nil {
			t.Fatalf("set codex binary: %v", err)
		}
		if got := app.providerBinaryPath(string(provider.Codex)); got != explicit {
			t.Fatalf("providerBinaryPath(codex) = %q, want the explicit %q", got, explicit)
		}
		if wrapper := app.codexWrapperFor(explicit); wrapper != "" {
			t.Fatalf("codexWrapperFor(%q) = %q, want none for an explicit path", explicit, wrapper)
		}
	}
}

// The harness mock outranks the resolution: harness mode never runs an
// installed Codex.
func TestHarnessCodexBinaryOverridesTheNativeResolution(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	writeProviderBinaryFile(t, f.native, "exit 0")
	app.codexLookPathFn = func(string) (string, error) { return f.command, nil }
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": "codex"}); err != nil {
		t.Fatalf("restore default codex setting: %v", err)
	}
	app.providerBinaryOverride = filepath.Join(t.TempDir(), "ao-mockprovider")

	if got := app.providerBinaryPath(string(provider.Codex)); got != app.providerBinaryOverride {
		t.Fatalf("providerBinaryPath(codex) = %q, want the harness mock", got)
	}
	if wrapper := app.codexWrapperFor(app.providerBinaryOverride); wrapper != "" {
		t.Fatalf("codexWrapperFor(mock) = %q, want none", wrapper)
	}
}

// The settings page reads which binary runs and why from the status: the
// version comes from the native binary, and the wrapper it replaced is named.
func TestProviderStatusesNameTheNativeBinaryAndItsWrapper(t *testing.T) {
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	mockexec.Write(t, f.native, "#!/bin/sh\necho 'codex-cli 0.162.1'\n")
	writeProviderBinaryFile(t, f.wrapper, "echo 'codex-cli 0.150.0-wrapper'")
	claude := mockexec.Write(t, filepath.Join(t.TempDir(), "claude"), "#!/bin/sh\necho '2.1.100 (Claude Code)'\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": claude}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	putCodexOnPath(t, app, f)

	statuses, err := app.GetProviderStatuses()
	if err != nil {
		t.Fatalf("GetProviderStatuses: %v", err)
	}
	var codexStatus *provider.ProviderStatus
	for i := range statuses {
		if statuses[i].Provider == string(provider.Codex) {
			codexStatus = &statuses[i]
		}
		if statuses[i].Provider == string(provider.Claude) && statuses[i].WrapperPath != "" {
			t.Fatalf("claude status names a wrapper: %+v", statuses[i])
		}
	}
	if codexStatus == nil {
		t.Fatalf("no codex status in %+v", statuses)
	}
	if codexStatus.BinaryPath != nativePath(t, f) || codexStatus.WrapperPath != realPath(t, f.wrapper) {
		t.Fatalf("codex status binary/wrapper = %q / %q, want %q / %q",
			codexStatus.BinaryPath, codexStatus.WrapperPath, nativePath(t, f), realPath(t, f.wrapper))
	}
	if !strings.Contains(codexStatus.Version, "0.162.1") {
		t.Fatalf("codex version = %q, want the native binary's answer", codexStatus.Version)
	}
}

// The default resolution walks PATH once and then answers from memory until
// something can have changed it: the binary watch tick, a change to the
// setting, a status read or a different PATH.
func TestDefaultCodexBinaryWalksPathOncePerInvalidation(t *testing.T) {
	app := newTestAppWithStore(t)
	stubProviderBinaryDetect(t, func(string) string { return "0.162.1" })
	// Serial: no codex on PATH at all, so the status read's own probe of the
	// bare default finds nothing either.
	t.Setenv("PATH", t.TempDir())
	claude := mockexec.Write(t, filepath.Join(t.TempDir(), "claude"), "#!/bin/sh\necho '2.1.100 (Claude Code)'\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": claude}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	walks := 0
	app.codexLookPathFn = func(string) (string, error) {
		walks++
		return "", exec.ErrNotFound
	}
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": "codex"}); err != nil {
		t.Fatalf("restore default codex setting: %v", err)
	}
	app.invalidateDefaultCodexBinary()
	read := func(want int, after string) {
		t.Helper()
		for range 100 {
			if got := app.providerBinaryPath(string(provider.Codex)); got != "codex" {
				t.Fatalf("providerBinaryPath(codex) = %q, want the bare default", got)
			}
		}
		if walks != want {
			t.Fatalf("after %s: %d PATH walks, want %d", after, walks, want)
		}
	}

	read(1, "100 reads with codex absent")
	app.sweepProviderBinaries()
	read(2, "a binary watch tick")
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"codexBinaryPath": "codex"}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	read(3, "a codexBinaryPath save")
	if _, err := app.GetProviderStatuses(); err != nil {
		t.Fatalf("GetProviderStatuses: %v", err)
	}
	read(4, "a provider status read")
	t.Setenv("PATH", t.TempDir())
	read(5, "a PATH change")
}

// A cached native binary that is gone, as after an npm uninstall or a Node
// version switch, is resolved again before anything spawns it.
func TestDefaultCodexBinaryDropsARemovedNativeBinary(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	writeProviderBinaryFile(t, f.wrapper, "exit 0")
	writeProviderBinaryFile(t, f.native, "exit 0")
	walks := 0
	app.codexLookPathFn = func(string) (string, error) {
		walks++
		return f.command, nil
	}
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": "codex"}); err != nil {
		t.Fatalf("restore default codex setting: %v", err)
	}
	app.invalidateDefaultCodexBinary()

	for range 10 {
		if got := app.providerBinaryPath(string(provider.Codex)); got != nativePath(t, f) {
			t.Fatalf("providerBinaryPath(codex) = %q, want the native binary", got)
		}
	}
	if walks != 1 {
		t.Fatalf("%d PATH walks for 10 reads of a present native binary, want 1", walks)
	}
	if err := os.Remove(f.native); err != nil {
		t.Fatal(err)
	}
	if got := app.providerBinaryPath(string(provider.Codex)); got != "codex" {
		t.Fatalf("providerBinaryPath(codex) = %q after the native binary was removed, want the bare default", got)
	}
	if walks != 2 {
		t.Fatalf("%d PATH walks, want the removal to force one more", walks)
	}
}
