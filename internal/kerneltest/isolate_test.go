package kerneltest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/textgen"
)

// recordingTB intercepts the two things the guard reports through: the cleanup
// it registers (so a test can run the tripwire deliberately) and Errorf (so the
// tripwire firing is an assertion instead of a failure). Everything else is the
// real *testing.T.
type recordingTB struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (r *recordingTB) Cleanup(fn func()) { r.cleanups = append(r.cleanups, fn) }

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// runCleanups runs the recorded cleanups LIFO, the way testing does.
func (r *recordingTB) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func TestDetachHomePointsAtAnEmptyDir(t *testing.T) {
	real, _ := os.UserHomeDir()

	home := DetachHome(t)

	if home == "" || home == real {
		t.Fatalf("DetachHome() = %q, want a temp dir distinct from %q", home, real)
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if got := os.Getenv(key); got != home {
			t.Errorf("%s = %q, want %q", key, got, home)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", home, err)
	}
	if len(entries) != 0 {
		t.Errorf("detached home is not empty: %v", entries)
	}
}

// TestDetachHomeClearsProviderHomeOverrides pins that the override variables
// which repoint a provider home AWAY from $HOME are removed by PRESENCE, not
// just blanked: Claude keys its macOS Keychain service off the variable
// existing at all, so an exported CLAUDE_CONFIG_DIR surviving DetachHome would
// hand a spawned child the developer's real credentials (security review
// 2026-08-25, finding 4).
func TestDetachHomeClearsProviderHomeOverrides(t *testing.T) {
	overrides := []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_HOME"}
	for _, key := range overrides {
		t.Setenv(key, "/tmp/developer-exported-"+key)
	}

	home := DetachHome(t)

	for _, key := range overrides {
		if got, present := os.LookupEnv(key); present {
			t.Errorf("%s survived DetachHome as %q; it must be UNSET (presence is what it means)", key, got)
		}
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "APPDATA", "LOCALAPPDATA"} {
		if got := os.Getenv(key); got != home {
			t.Errorf("%s = %q, want the detached home %q", key, got, home)
		}
	}
}

func TestPoisonProviderBinaryRecordsSpawnAndFailsTheTest(t *testing.T) {
	rec := &recordingTB{TB: t}
	poison, sentinel := PoisonProviderBinary(rec)

	if filepath.Base(sentinel) != SentinelName {
		t.Errorf("sentinel = %q, want basename %q", sentinel, SentinelName)
	}
	info, err := os.Stat(poison)
	if err != nil {
		t.Fatalf("stat poisoned binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("poisoned binary mode = %v, want executable", info.Mode().Perm())
	}

	// A clean run must not fail the test.
	rec.runCleanups()
	if len(rec.errors) != 0 {
		t.Fatalf("tripwire fired without a spawn: %v", rec.errors)
	}

	// Spawning it records the argv, writes to stderr, and exits 127.
	cmd := exec.Command(poison, "--dangerously-skip-permissions", "-p")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
		t.Fatalf("poisoned binary exit = %v, want exit status 127", err)
	}
	if !strings.Contains(stderr.String(), "refused to spawn a provider binary without a mock") {
		t.Errorf("poisoned binary stderr = %q, want the refusal notice", stderr.String())
	}
	recorded, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(recorded), "--dangerously-skip-permissions") {
		t.Errorf("sentinel = %q, want the spawn argv", recorded)
	}

	// Now the tripwire must fail the test, naming the recorded spawn.
	rec.errors = nil
	rec.runCleanups()
	if len(rec.errors) != 1 {
		t.Fatalf("tripwire errors = %v, want exactly one failure", rec.errors)
	}
	if !strings.Contains(rec.errors[0], "spawned a provider binary without a mock") ||
		!strings.Contains(rec.errors[0], "--dangerously-skip-permissions") {
		t.Errorf("tripwire message = %q, want the instruction plus the argv", rec.errors[0])
	}
}

func TestIsolateSpawnsInstallsBothLayers(t *testing.T) {
	isolation := IsolateSpawns(t)

	if got := os.Getenv("HOME"); got != isolation.Home {
		t.Errorf("HOME = %q, want %q", got, isolation.Home)
	}
	if _, err := os.Stat(isolation.PoisonedBinary); err != nil {
		t.Errorf("poisoned binary missing: %v", err)
	}
	if isolation.Sentinel == "" || filepath.Dir(isolation.Sentinel) != filepath.Dir(isolation.PoisonedBinary) {
		t.Errorf("sentinel %q should sit beside the poison %q", isolation.Sentinel, isolation.PoisonedBinary)
	}
}

func TestProviderBinarySettingsCoversBothProviders(t *testing.T) {
	patch := ProviderBinarySettings("/poison")

	if len(patch) != 2 {
		t.Fatalf("patch = %v, want exactly the two binary keys", patch)
	}
	for _, key := range []string{"claudeBinaryPath", "codexBinaryPath"} {
		if patch[key] != "/poison" {
			t.Errorf("%s = %v, want /poison", key, patch[key])
		}
	}
}

func TestDisabledCodexModelCatalogNeverSpawns(t *testing.T) {
	models, err := DisabledCodexModelCatalog().Get(context.Background(), "codex")
	if err == nil {
		t.Fatalf("Get() error = nil, want the disabled-catalog error (models=%v)", models)
	}
	if !strings.Contains(err.Error(), "live Codex catalog disabled") {
		t.Errorf("Get() error = %v, want the disabled-catalog error", err)
	}
}

func TestStubTextGenerationExecutorAlwaysErrors(t *testing.T) {
	_, err := StubTextGenerationExecutor()(context.Background(), textgen.CLISpec{})
	if err == nil {
		t.Fatal("stub executor error = nil, want the stubbed-generation error")
	}
	if !strings.Contains(err.Error(), "text generation is stubbed in tests") {
		t.Errorf("stub executor error = %v, want the stubbed-generation error", err)
	}
}
