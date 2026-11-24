package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/textgen"
)

// agyLaunch is one launch the mock agy recorded.
type agyLaunch struct {
	args, store, geminiHome, appDataDir string
}

func readAgyLaunches(t *testing.T, record string) []agyLaunch {
	t.Helper()
	data, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the mock agy's launch record: %v", err)
	}
	var launches []agyLaunch
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := map[string]string{}
		for _, part := range strings.Split(line, "|") {
			name, value, _ := strings.Cut(part, "=")
			fields[name] = value
		}
		launches = append(launches, agyLaunch{fields["args"], fields["store"], fields["gemini_home"], fields["app_data_dir"]})
	}
	return launches
}

// agyLaunchRig is an app whose agy binary records every launch, with a file
// account store under the app's provider home.
type agyLaunchRig struct {
	app    *App
	record string
	// named is the argv of a canonical `agy acp` while the process HOME is
	// not the app's provider home: the manager's directory, named.
	named string
}

func newAgyLaunchRig(t *testing.T) agyLaunchRig {
	t.Helper()
	app, _ := setupE2EApp(t)
	attachFileAccountStoresForTest(t, app)
	record := filepath.Join(t.TempDir(), "launches.txt")
	binary := writePoolAgyBinary(t, "", record)
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	geminiDir := filepath.Join(app.credentialHomeOverride, ".gemini")
	return agyLaunchRig{app: app, record: record, named: "acp --gemini-dir " + geminiDir + " --app-data-dir " + agy.DefaultAppDataName}
}

// newLaunches is what the mock recorded after the first since launches.
func (r agyLaunchRig) newLaunches(t *testing.T, since int) []agyLaunch {
	t.Helper()
	return readAgyLaunches(t, r.record)[since:]
}

// wantLaunch fails unless launches is exactly one launch with args on the
// file store (the app's account store keeps the login in a file) with no
// inherited home variable.
func wantLaunch(t *testing.T, what string, launches []agyLaunch, args string) {
	t.Helper()
	if len(launches) != 1 {
		t.Fatalf("%s launched %d agy processes %v, want one", what, len(launches), launches)
	}
	got := launches[0]
	if got.args != args || got.store != "1" || got.geminiHome != "<unset>" || got.appDataDir != "<unset>" {
		t.Fatalf("%s launched %+v, want args %q on the file store with no home variable", what, got, args)
	}
}

func (r agyLaunchRig) startAgyThread(t *testing.T) {
	t.Helper()
	thread, err := createTestThread(t, r.app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
}

func (r agyLaunchRig) probeCatalog(t *testing.T) {
	t.Helper()
	r.app.providerDiscoveryService().RefreshAgyModelCatalog()
	if _, ok := r.app.providerDiscoveryService().AgyModelsForBinary(context.Background(), r.app.providerBinaryPath(string(provider.Agy))); !ok {
		t.Fatal("the agy model catalog probe answered nothing")
	}
}

// A pooled agy server runs on the canonical home decided as it starts, and
// that home is part of its identity: a session start and a catalog probe on
// another home never reuse it, and on the same home they do. The home
// changes here with the process HOME (the manager's directory is named while
// HOME is elsewhere, and agy's default once HOME holds it), as the store
// changes when a login outside the app adds or removes the Keychain item.
// Imports borrow a server only on their home, and another directory's
// import runs on that directory.
//
// Serial: it sets HOME.
func TestAgyServersAreKeyedByTheHomeTheyRunOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := newAgyLaunchRig(t)
	rig.app.providerDiscoveryCaches.AgyModels = agy.NewCatalogCache()
	ctx := context.Background()

	rig.startAgyThread(t)
	wantLaunch(t, "a session start", rig.newLaunches(t, 0), rig.named)
	rig.probeCatalog(t)
	if got := rig.newLaunches(t, 1); len(got) != 0 {
		t.Fatalf("a catalog probe on the server's home launched %v, want it to borrow the server", got)
	}

	t.Setenv("HOME", rig.app.credentialHomeOverride)
	rig.probeCatalog(t)
	wantLaunch(t, "a catalog probe after the home changed", rig.newLaunches(t, 1), "acp")
	rig.startAgyThread(t)
	wantLaunch(t, "a session start after the home changed", rig.newLaunches(t, 2), "acp")
	if _, err := rig.app.listAgyImportSessions(ctx, ""); err != nil {
		t.Fatalf("canonical import: %v", err)
	}
	if got := rig.newLaunches(t, 3); len(got) != 0 {
		t.Fatalf("a canonical import launched %v, want it to borrow the server on its home", got)
	}
	other := filepath.Join(t.TempDir(), "gemini", agy.DefaultAppDataName)
	if _, err := rig.app.listAgyImportSessions(ctx, other); err != nil {
		t.Fatalf("import from another directory: %v", err)
	}
	wantLaunch(t, "an import from another directory", rig.newLaunches(t, 3), "acp --gemini-dir "+filepath.Dir(other)+" --app-data-dir "+agy.DefaultAppDataName)
}

// An account probe, `agy mcp list` and text generation run on the
// manager's store and home. `mcp list` and `-p` take no home directory, so
// while HOME does not hold the manager's directory they refuse to run
// rather than read another home's login.
//
// Serial: it sets HOME.
func TestAgyProbesMCPAndTextGenerationRunOnTheManagersHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := newAgyLaunchRig(t)
	ctx := context.Background()
	var specs []textgen.CLISpec
	rig.app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		specs = append(specs, spec)
		return textgen.CLIResult{Stdout: `{"result":"{}"}`}, nil
	}
	rig.app.lookPathFn = fakeLookPath(rig.app.providerBinaryPath(string(provider.Agy)))
	if _, err := rig.app.settings.Update(map[string]any{"textGenerationProvider": string(provider.Agy)}); err != nil {
		t.Fatal(err)
	}
	primary, err := rig.app.resolveTextGenerationConfig(ctx)
	if err != nil || primary.Provider != string(provider.Agy) {
		t.Fatalf("text generation config = %+v, %v; want agy", primary, err)
	}
	fallback, ok := rig.app.resolveTextGenerationConfigFor(ctx, string(provider.Agy))
	if !ok {
		t.Fatal("no fallback text generation config for agy")
	}
	runTextGen := func(cfg textgen.Config) error {
		_, err := textgen.RunAgy(ctx, cfg, t.TempDir(), `{"type":"object"}`, nil, "prompt", 0)
		return err
	}

	// HOME elsewhere: the probe names the manager's directory; the others
	// refuse.
	_, _ = rig.app.providerDiscoveryService().ProbeAgyAccount()
	wantLaunch(t, "an account probe", rig.newLaunches(t, 0), rig.named)
	if _, err := rig.app.RefreshMcpServerStatus(string(provider.Agy), ""); err == nil || !strings.Contains(err.Error(), "default home") {
		t.Fatalf("agy mcp list under another HOME = %v, want a refusal", err)
	}
	for name, cfg := range map[string]textgen.Config{"primary": primary, "fallback": fallback} {
		if err := runTextGen(cfg); err == nil || !strings.Contains(err.Error(), "default home") {
			t.Fatalf("%s text generation under another HOME = %v, want a refusal", name, err)
		}
	}
	if got := rig.newLaunches(t, 1); len(got) != 0 || len(specs) != 0 {
		t.Fatalf("refused commands launched %v and %d text generation runs", got, len(specs))
	}

	// HOME holds the manager's directory: agy's default home, on the file
	// store.
	t.Setenv("HOME", rig.app.credentialHomeOverride)
	if _, err := rig.app.RecheckAgyAccount(); err != nil && len(rig.newLaunches(t, 1)) == 0 {
		t.Fatalf("account recheck: %v", err)
	}
	wantLaunch(t, "an account recheck", rig.newLaunches(t, 1), "acp")
	if _, err := rig.app.RefreshMcpServerStatus(string(provider.Agy), ""); err != nil {
		t.Fatalf("agy mcp list: %v", err)
	}
	wantLaunch(t, "agy mcp list", rig.newLaunches(t, 2), "mcp list")
	for name, cfg := range map[string]textgen.Config{"primary": primary, "fallback": fallback} {
		specs = nil
		if err := runTextGen(cfg); err != nil {
			t.Fatalf("%s text generation: %v", name, err)
		}
		if len(specs) != 1 {
			t.Fatalf("%s text generation ran %d times", name, len(specs))
		}
		spec := specs[0]
		if spec.Env.Pins[provider.AgyForceFileStorageEnv] != "1" || !slices.Contains(spec.Env.Clears, provider.AgyGeminiHomeEnv) || !slices.Contains(spec.Env.Clears, provider.AgyAppDataDirEnv) || slices.Contains(spec.Args, "--gemini-dir") {
			t.Fatalf("%s text generation ran with pins %v, clears %v, args %v; want the file store on agy's default home", name, spec.Env.Pins, spec.Env.Clears, spec.Args)
		}
	}
}

// A server's key names the store and directory it reads the login from, so
// servers on the file store, on the Keychain, and on a named directory never
// share one.
func TestAnAgyPoolKeyNamesTheServersHome(t *testing.T) {
	t.Parallel()
	homes := map[string]func() (agy.Home, error){
		"file":     func() (agy.Home, error) { return agy.CanonicalHome("", true) },
		"keychain": func() (agy.Home, error) { return agy.CanonicalHome("", false) },
		"directory": func() (agy.Home, error) {
			return agy.CanonicalHome(filepath.Join(t.TempDir(), "antigravity-cli"), true)
		},
	}
	keys := map[string]string{}
	for name, build := range homes {
		home, err := build()
		if err != nil {
			t.Fatal(err)
		}
		key := agyPoolKey("agy", providerBinaryIdentity{}, "account", 1, home, provider.EnvInput{}, "")
		if other, taken := keys[key]; taken {
			t.Fatalf("servers on the %s and %s homes share a key", name, other)
		}
		keys[key] = name
	}
}

// notACPConn is a pooled connection that is not an agy ACP server.
type notACPConn struct{ closed chan struct{} }

func (c *notACPConn) PID() int              { return 0 }
func (c *notACPConn) Done() <-chan struct{} { return nil }
func (c *notACPConn) Close() error          { close(c.closed); return nil }

// A connection under the agy key that is not an ACP server is not borrowed,
// and the borrow hands its reference back, so the pool still closes it once
// it idles.
func TestAnAgyBorrowReleasesAConnectionThatIsNotACP(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	attachFileAccountStoresForTest(t, app)
	binary := writePoolAgyBinary(t, "", "")
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	home, err := app.ensureProviderAccountManager().AgyCanonicalHome(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := app.resolveProviderBinaryIdentity(string(provider.Agy))
	selection := app.captureProviderAccountSelection(string(provider.Agy))
	key := agyPoolKey(agy.ResolveBinary(binary), identity, selection.AccountID, selection.Generation, home, app.sessionProcessEnv(string(provider.Agy)), "")
	fake := &notACPConn{closed: make(chan struct{})}
	_, release, err := app.sessionManager().runtime.Connections().Acquire(ctx, key, 50*time.Millisecond, func(context.Context) (provider.Connection, error) {
		return fake, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if conn, _ := app.borrowAgyConnForCurrentAccount(binary, home); conn != nil {
		t.Fatal("borrowed a connection that is not an ACP server")
	}
	release()
	select {
	case <-fake.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool kept a connection the borrow never handed back")
	}
}
