//go:build !providersmoke

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/power"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerdiscoveryapp"
	"agent-overflow/internal/settings"
)

// detachTestProcessHome is TestMain's half of the isolation: no test in this
// binary may see the developer's provider homes or live settings. Detaching
// once per process, rather than per test, is what lets the fixtures below run
// under t.Parallel.
func detachTestProcessHome() (remove func() error, err error) {
	return kerneltest.DetachProcessHome()
}

// TestTheTestProcessHomeIsDetached proves TestMain detached the home before
// any test ran, so a test that never builds a fixture still cannot resolve the
// developer's home.
func TestTheTestProcessHomeIsDetached(t *testing.T) {
	t.Parallel()
	isolation := kerneltest.IsolateProcessSpawns(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if home != isolation.Home {
		t.Fatalf("UserHomeDir() = %q, want the detached process home %q", home, isolation.Home)
	}
}

// A fixture's App that outlives its settings directory reads every setting
// at its default. Its provider binaries are then the fixture's poison, never
// a bare name an exec would resolve on PATH.
func TestAFixtureWithoutItsSettingsResolvesNoProviderOnPath(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	if err := os.RemoveAll(app.configDir); err != nil {
		t.Fatal(err)
	}
	if got := app.currentSettings().CodexBinaryPath; got != settings.DefaultSettings.CodexBinaryPath {
		t.Fatalf("settings without their directory read codexBinaryPath %q, want the default", got)
	}
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		if got := app.providerBinaryPath(name); !filepath.IsAbs(got) {
			t.Fatalf("providerBinaryPath(%s) = %q at the default setting, want the fixture's poisoned binary", name, got)
		}
	}
}

// isolateE2EProviderSpawns makes it structurally impossible for a test built on
// setupE2EApp (or newTestAppWithStore) to reach a real provider binary or the
// developer's real provider homes. The reusable half — the poisoned binary,
// its spawn tripwire, the detached HOME, and the two side-effect stubs — lives
// in internal/kerneltest so a fixture in ANY package gets the same guard;
// kerneltest's package guide has the rationale and the incident history. What
// stays here is only what needs *App: writing the poison into the App's
// settings service and installing the stubs on the App's seams.
//
// The providersmoke build supplies a no-op twin: that gate exists precisely to
// exercise default binary resolution against the real CLIs, opts in via
// `make provider-smoke`, and documents that it spends real tokens.
func isolateE2EProviderSpawns(t *testing.T, app *App) {
	t.Helper()

	isolation := kerneltest.IsolateProcessSpawns(t)
	// Repository identity enrichment must not run a real forge CLI.
	app.forgeCLIs.isolated = true
	// The process home is shared by every test. Each fixture gets its own
	// provider home through the seam every app-layer provider path resolves
	// through (providerHome), so provider state one test writes is never
	// another's.
	app.credentialHomeOverride = t.TempDir()
	if _, err := app.settings.Update(kerneltest.ProviderBinarySettings(isolation.PoisonedBinary)); err != nil {
		t.Fatalf("poison provider binary settings: %v", err)
	}
	// A setting at its default, including every setting once the fixture's
	// settings directory is removed, runs the poison rather than a bare name
	// on PATH. Tests of default resolution clear it
	// (resetProviderBinarySettings, putCodexOnPath).
	app.defaultProviderBinaryOverride = isolation.PoisonedBinary
	// A test that restores the default `codex` setting must not have it
	// resolved to the developer's installed native binary. Tests of that
	// resolution install their own lookup over this.
	app.codexLookPathFn = func(string) (string, error) { return "", exec.ErrNotFound }
	// Claude account identification and usage send a credential's bearer to
	// the API. The isolated boot's transport answers the profile from the
	// token (claudefake) and refuses the rest; a test asserting usage
	// installs its own client over this.
	app.rateLimitProbeClientOverride = isolatedHTTPClient

	// The live Codex model catalog spawns `codex app-server` for `model/list`
	// as a side effect of ordinary thread creation. Disable it the same way
	// newTestAppWithStore does; a test asserting catalog behavior assigns
	// app.providerDiscoveryCaches its own codexmodels.NewWith over this.
	defaultCaches := providerdiscoveryapp.DefaultCaches()
	app.providerDiscoveryCaches = &providerdiscoveryapp.Caches{
		Claude:      defaultCaches.Claude,
		Codex:       defaultCaches.Codex,
		CodexModels: kerneltest.DisabledCodexModelCatalog(),
	}

	// Text generation (thread titles, commit messages) shells out to a real
	// provider CLI as a side effect of ordinary sends — it must never do so
	// incidentally from a test. A test asserting generated text installs its
	// own fake over this.
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	// A test's Claude CLI reports its models only when the test stores a
	// report, so text generation on Claude would wait the probe's whole
	// timeout for one that never comes. It gives up at once instead; a test
	// of the wait sets its own bound.
	app.claudeModelsWaitForTest = time.Nanosecond

	// A provider update check runs `claude doctor` and reads the release
	// hosts; a run replaces the provider CLI. The runner is stubbed and the
	// hosts are answered by the fake feed an isolated boot gets. A test of
	// update runs installs its own runner over this.
	isolateProviderUpdates(app)

	// The OS sleep inhibitor is real system state — a fixture that flips
	// keepAwakeEnabled must not pin the developer's machine awake for the
	// rest of the session. internal/power refuses inside a test binary on
	// its own, so this seam is about keeping the log quiet and giving a
	// test asserting the mode something to install a recorder over.
	app.keepAwakeApply = func(power.Mode) error { return nil }
}
