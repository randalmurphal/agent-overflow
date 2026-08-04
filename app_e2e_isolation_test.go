//go:build !providersmoke

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/textgen"
)

// isolateE2EProviderSpawns makes it structurally impossible for a test built on
// setupE2EApp to reach a real provider binary or the developer's real provider
// homes. Mocking is NOT opt-in: before this guard existed, any code path that
// started a session without an explicit mock (the workflow wake delivery was
// the live example) silently resolved `claude` from PATH and ran a real,
// billed API turn against the developer's real `~/.claude` credentials — then
// test teardown killed it, sometimes mid token-refresh, which consumes the
// single-use refresh token without persisting the rotation and destroys the
// login hours later (incident 2026-08-03; 143 leaked sessions since Jul 25).
//
// Two layers, so a miss in one is caught by the other:
//
//  1. Both provider binary settings point at a poison script that records the
//     spawn and exits. The test then FAILS from cleanup with instructions —
//     a test that needs a live session must install a mock
//     (testutil.WriteMockClaudeScript / WriteMockCodexSession) over the poison.
//  2. HOME/USERPROFILE point at an empty temp dir, so anything that still
//     reaches a real binary (or reads a provider home directly) finds no
//     credentials and no session history.
//
// The providersmoke build supplies a no-op twin: that gate exists precisely to
// exercise default binary resolution against the real CLIs, opts in via
// `make provider-smoke`, and documents that it spends real tokens.
func isolateE2EProviderSpawns(t *testing.T, app *App) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "real-provider-spawn-attempted")
	poison := filepath.Join(dir, "poisoned-provider-binary")
	script := "#!/bin/sh\n" +
		"echo \"argv: $0 $*\" >> '" + sentinel + "'\n" +
		"echo 'agent-overflow tests: refused to spawn a provider binary without a mock' >&2\n" +
		"exit 127\n"
	if err := os.WriteFile(poison, []byte(script), 0o755); err != nil {
		t.Fatalf("write poisoned provider binary: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": poison,
		"codexBinaryPath":  poison,
	}); err != nil {
		t.Fatalf("poison provider binary settings: %v", err)
	}

	// The live Codex model catalog spawns `codex app-server` for `model/list`
	// as a side effect of ordinary thread creation. Disable it the same way
	// newTestAppWithStore does; a test asserting catalog behavior assigns
	// app.codexModelCatalog its own codexmodels.NewWith over this.
	app.codexModelCatalogOnce.Do(func() {
		app.codexModelCatalog = codexmodels.NewWith(
			time.Minute,
			func(context.Context, string) ([]provider.ModelInfo, error) {
				return nil, errors.New("live Codex catalog disabled in App tests")
			},
			time.Now,
		)
	})

	// Text generation (thread titles, commit messages) shells out to a real
	// provider CLI as a side effect of ordinary sends — it must never do so
	// incidentally from a test. The flows treat an executor error as a failed
	// generation and carry on; a test asserting generated text installs its own
	// fake over this, exactly as the textgen tests already do.
	app.textGenerationExecutor = func(context.Context, textgen.CLISpec) (textgen.CLIResult, error) {
		return textgen.CLIResult{}, errors.New(
			"text generation is stubbed in tests; assign app.textGenerationExecutor a fake to exercise it",
		)
	}

	// Registered before setupE2EApp's session teardown, so (LIFO) it runs after
	// every session is closed and no spawn is still in flight.
	t.Cleanup(func() {
		invocations, err := os.ReadFile(sentinel)
		if err != nil {
			return
		}
		t.Errorf(
			"test spawned a provider binary without a mock; install one over the poisoned default "+
				"(testutil.WriteMockClaudeScript / WriteMockCodexSession via settings claudeBinaryPath/codexBinaryPath) "+
				"— tests must never resolve real provider CLIs (see internal/AGENTS.md 'Testing bar')\nspawns:\n%s",
			invocations,
		)
	})
}
