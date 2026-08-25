//go:build !providersmoke

package main

import (
	"testing"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/power"
)

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

	isolation := kerneltest.IsolateSpawns(t)
	if _, err := app.settings.Update(kerneltest.ProviderBinarySettings(isolation.PoisonedBinary)); err != nil {
		t.Fatalf("poison provider binary settings: %v", err)
	}

	// The live Codex model catalog spawns `codex app-server` for `model/list`
	// as a side effect of ordinary thread creation. Disable it the same way
	// newTestAppWithStore does; a test asserting catalog behavior assigns
	// app.codexModelCatalog its own codexmodels.NewWith over this.
	app.codexModelCatalogOnce.Do(func() {
		app.codexModelCatalog = kerneltest.DisabledCodexModelCatalog()
	})

	// Text generation (thread titles, commit messages) shells out to a real
	// provider CLI as a side effect of ordinary sends — it must never do so
	// incidentally from a test. A test asserting generated text installs its
	// own fake over this.
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()

	// The OS sleep inhibitor is real system state — a fixture that flips
	// keepAwakeEnabled must not pin the developer's machine awake for the
	// rest of the session. internal/power refuses inside a test binary on
	// its own, so this seam is about keeping the log quiet and giving a
	// test asserting the mode something to install a recorder over.
	app.keepAwakeApply = func(power.Mode) error { return nil }
}
