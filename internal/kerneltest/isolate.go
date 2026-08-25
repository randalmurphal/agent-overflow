package kerneltest

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

// SentinelName is the file the poisoned provider binary appends every spawn
// attempt to. Exported so a fixture that needs to inspect the record (rather
// than only fail on it) does not have to hardcode the name.
const SentinelName = "real-provider-spawn-attempted"

// Isolation is what IsolateSpawns installed: the empty HOME every provider
// lookup now resolves against, and the poison script both provider binary
// settings must point at.
type Isolation struct {
	// Home is the empty temp directory HOME and USERPROFILE were pointed at.
	Home string
	// PoisonedBinary is the script to install as claudeBinaryPath and
	// codexBinaryPath. Spawning it records the argv and exits 127; the
	// registered tripwire then fails the test.
	PoisonedBinary string
	// Sentinel is the file PoisonedBinary appends spawn attempts to.
	Sentinel string
}

// IsolateSpawns makes it structurally impossible for a test to reach a real
// provider binary or the developer's real provider homes. Mocking is NOT
// opt-in: before this guard existed, any code path that started a session
// without an explicit mock (the workflow wake delivery was the live example)
// silently resolved `claude` from PATH and ran a real, billed API turn against
// the developer's real `~/.claude` credentials — then test teardown killed it,
// sometimes mid token-refresh, which consumes the single-use refresh token
// without persisting the rotation and destroys the login hours later (incident
// 2026-08-03; 143 leaked sessions since Jul 25).
//
// Two layers, so a miss in one is caught by the other:
//
//  1. Both provider binary settings point at a poison script that records the
//     spawn and exits. The test then FAILS from cleanup with instructions —
//     a test that needs a live session must install a mock
//     (testutil.WriteMockClaudeScript / WriteMockCodexSession) over the poison.
//  2. HOME/USERPROFILE point at an empty temp dir and the provider-home
//     override variables (CLAUDE_CONFIG_DIR, CLAUDE_SECURESTORAGE_CONFIG_DIR,
//     CODEX_HOME) are unset, so anything that still reaches a real binary (or
//     reads a provider home directly) finds no credentials and no session
//     history — even under a developer shell that exports an override.
//
// Installing the returned PoisonedBinary into whatever settings/config the
// fixture's subject resolves its binaries from is the caller's job — that is
// the only part of the guard this package cannot do for you. Stubbing the
// side-effect spawn paths (DisabledCodexModelCatalog,
// StubTextGenerationExecutor) is likewise caller-side, because the seams live
// on the subject.
func IsolateSpawns(t testing.TB) Isolation {
	t.Helper()

	home := DetachHome(t)
	poison, sentinel := PoisonProviderBinary(t)
	return Isolation{Home: home, PoisonedBinary: poison, Sentinel: sentinel}
}

// DetachHome points HOME and USERPROFILE at an empty temp dir for the rest of
// the test, so anything reading a provider home directly (credentials, session
// history, ~/.claude.json) finds nothing rather than the developer's real one.
// Returns the directory.
//
// The provider-home OVERRIDE variables are unset outright, not pointed at the
// temp dir: they repoint a provider home AWAY from $HOME (see
// settings/providerenv.go), so a developer shell that exports
// CLAUDE_CONFIG_DIR would hand every spawned child the real credentials no
// matter where HOME points — and on macOS, Claude >= 2.1.220 keys its
// Keychain service off the variable's PRESENCE, so even an empty value is not
// equivalent to absence. t.Setenv first so the original value is restored
// after the test.
//
// XDG_*/APPDATA are detached to the temp dir for the same reason at one
// remove: os.UserConfigDir honors them, so a fixture exercising the real boot
// path would otherwise read and write the developer's live agent-overflow
// settings and database.
func DetachHome(t testing.TB) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(key, "") // registers the restore for after the test
		os.Unsetenv(key)  // presence, not value, is what these mean
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, home)
	}
	return home
}

// PoisonProviderBinary writes an executable script that records its argv and
// exits 127, and registers the tripwire that fails the test if anything ever
// spawned it. Returns the script path and the sentinel file it records to.
//
// The tripwire is registered here, before the caller registers its own session
// teardown, so (LIFO) it runs after every session is closed and no spawn is
// still in flight.
func PoisonProviderBinary(t testing.TB) (poison, sentinel string) {
	t.Helper()

	dir := t.TempDir()
	sentinel = filepath.Join(dir, SentinelName)
	poison = filepath.Join(dir, "poisoned-provider-binary")
	script := "#!/bin/sh\n" +
		"echo \"argv: $0 $*\" >> '" + sentinel + "'\n" +
		"echo 'agent-overflow tests: refused to spawn a provider binary without a mock' >&2\n" +
		"exit 127\n"
	if err := os.WriteFile(poison, []byte(script), 0o755); err != nil {
		t.Fatalf("write poisoned provider binary: %v", err)
	}

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
	return poison, sentinel
}

// ProviderBinarySettings is the settings patch that points both provider
// binaries at the poison script. One helper so no fixture can poison Claude
// and forget Codex.
func ProviderBinarySettings(poison string) map[string]any {
	return map[string]any{
		"claudeBinaryPath": poison,
		"codexBinaryPath":  poison,
	}
}

// DisabledCodexModelCatalog returns a Codex model catalog whose lookups fail
// instead of spawning. The live catalog spawns `codex app-server` for
// `model/list` as a side effect of ordinary thread creation; a test asserting
// catalog behavior assigns its own codexmodels.NewWith over this.
func DisabledCodexModelCatalog() *codexmodels.Cache {
	return codexmodels.NewWith(
		time.Minute,
		func(context.Context, string) ([]provider.ModelInfo, error) {
			return nil, errors.New("live Codex catalog disabled in App tests")
		},
		time.Now,
	)
}

// StubTextGenerationExecutor returns an executor that always errors. Text
// generation (thread titles, commit messages) shells out to a real provider
// CLI as a side effect of ordinary sends — it must never do so incidentally
// from a test. The flows treat an executor error as a failed generation and
// carry on; a test asserting generated text installs its own fake over this,
// exactly as the textgen tests already do.
func StubTextGenerationExecutor() textgen.CLIExecutor {
	return func(context.Context, textgen.CLISpec) (textgen.CLIResult, error) {
		return textgen.CLIResult{}, errors.New(
			"text generation is stubbed in tests; assign app.textGenerationExecutor a fake to exercise it",
		)
	}
}
