package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

func (m *Manager) providerProbeCacheKey(providerName, binary string) provider.ProbeCacheKey {
	return m.providerProbeCacheKeyForAccount(providerName, binary, m.Selection(providerName).AccountID)
}

func (m *Manager) providerProbeCacheKeyForAccount(providerName, binary, accountID string) provider.ProbeCacheKey {
	return providerProbeCacheKeyForAccountEnv(binary, accountID, m.providerCustomEnv(providerName))
}

// ProbeCacheKey returns the canonical-home identity cache key.
func (m *Manager) ProbeCacheKey(providerName, binary string) provider.ProbeCacheKey {
	return m.providerProbeCacheKey(providerName, binary)
}

// ProbeCacheKeyForAccount returns an explicit managed-account identity key.
func (m *Manager) ProbeCacheKeyForAccount(providerName, binary, accountID string) provider.ProbeCacheKey {
	return m.providerProbeCacheKeyForAccount(providerName, binary, accountID)
}

// providerProbeCacheKeyForAccountEnv builds the key from an explicit custom
// environment. Separate from the App method so the settings-mutation path can
// build the key for the PREVIOUS environment and evict it — see
// invalidateProviderProbeCacheForEnvChange.
func providerProbeCacheKeyForAccountEnv(binary, accountID string, customEnv map[string]string) provider.ProbeCacheKey {
	return provider.ProbeCacheKey{
		Binary:         binary,
		AccountID:      accountID,
		WorkDir:        providerProbeWorkDir(),
		EnvFingerprint: provider.EnvFingerprint(customEnv),
	}
}

// ProbeCacheKeyForAccountEnv builds a key for a settings snapshot that is no
// longer current, allowing root's settings mutation path to evict it.
func ProbeCacheKeyForAccountEnv(binary, accountID string, customEnv map[string]string) provider.ProbeCacheKey {
	return providerProbeCacheKeyForAccountEnv(binary, accountID, customEnv)
}

// providerCustomEnv is the user's configured environment for a provider, as
// the override map every spawn path consumes. nil when nothing is configured.
//
// claude-tui resolves to Claude's list (one binary, one backend); unknown
// providers get nil.
func (m *Manager) providerCustomEnv(providerName string) map[string]string {
	return m.currentSettings().ProviderEnvMap(providerName)
}

// providerProbeEnv merges the user's custom environment with the pins a
// specific probe needs (a temporary CLAUDE_CONFIG_DIR / CODEX_HOME). Pins win:
// they name the credential home the probe exists to interrogate, and the
// settings layer already refuses to let a user configure those names.
//
// Returns nil when both sides are empty so probes keep their "inherit the
// process environment" fast path.
func (m *Manager) providerProbeEnv(providerName string, pins map[string]string) map[string]string {
	return probeEnvWith(m.providerCustomEnv(providerName), pins)
}

// probeEnvWith is providerProbeEnv for a custom environment already read.
func probeEnvWith(custom, pins map[string]string) map[string]string {
	if len(custom) == 0 && len(pins) == 0 {
		return nil
	}
	merged := make(map[string]string, len(custom)+len(pins))
	for k, v := range custom {
		merged[k] = v
	}
	for k, v := range pins {
		merged[k] = v
	}
	return merged
}

// ProbeEnv merges current user configuration with manager-owned credential
// home pins for the few root leaf probes that add provider-specific hooks.
func (m *Manager) ProbeEnv(providerName string, pins map[string]string) map[string]string {
	return m.providerProbeEnv(providerName, pins)
}

// providerLoginEnv is providerProbeEnv with the boot-mode layer a sign-in
// spawn carries between the user's configuration and the pins. It exists
// because a sign-in is the one provider process besides a session with a
// lifecycle something outside it has to steer, which is what Deps.LoginSpawnEnv
// hands it the address of; a probe is one shot and has nothing to steer.
//
// Precedence is custom < boot mode < pins, the same ladder sessionProcessEnv
// uses and for the same reasons: a deliberate user override outranks AO's
// defaults, a boot contract outranks a preference, and the pin naming the
// isolated login home outranks everything because the flow must not land in
// the canonical one.
func (m *Manager) providerLoginEnv(providerName string, pins map[string]string) map[string]string {
	var boot map[string]string
	if m.deps.LoginSpawnEnv != nil {
		boot = m.deps.LoginSpawnEnv()
	}
	if len(boot) == 0 {
		return m.providerProbeEnv(providerName, pins)
	}
	custom := m.providerCustomEnv(providerName)
	merged := make(map[string]string, len(custom)+len(boot)+len(pins))
	maps.Copy(merged, custom)
	maps.Copy(merged, boot)
	maps.Copy(merged, pins)
	return merged
}

// claudeProbeConfig is the ONE constructor for a Claude probe invocation:
// binary, the pinned probe workdir, and the user's custom environment merged
// under whatever config-home pin the caller needs. Callers that need a
// non-default timeout or an OnSnapshot hook set those on the returned value.
//
// Constructing ProbeConfig literals at call sites is what let the custom
// environment reach the session but not the probe — which is precisely the
// failure this feature must not have, because an ANTHROPIC_BASE_URL that
// changes which backend answers must change who the probe reports.
func (m *Manager) claudeProbeConfig(binary string, pins map[string]string) claude.ProbeConfig {
	return m.claudeProbeConfigWith(binary, m.providerCustomEnv(string(provider.Claude)), pins)
}

// claudeProbeConfigWith is claudeProbeConfig for a custom environment
// already read.
func (m *Manager) claudeProbeConfigWith(binary string, custom, pins map[string]string) claude.ProbeConfig {
	return claude.ProbeConfig{
		Binary:         binary,
		WorkDir:        providerProbeWorkDir(),
		Env:            probeEnvWith(custom, pins),
		Reaper:         m.deps.Reaper,
		ReadCredential: m.claudeProbeCredentialReader(pins),
		// Sent to a probe of an isolated home too, where it has nothing to
		// keep: one rule for every probe.
		Retention: m.claudeRetention(providerProbeWorkDir()),
	}
}

// claudeRetention is the zero Retention, which refuses the probe, when the
// dependency is missing.
func (m *Manager) claudeRetention(workDir string) claude.Retention {
	if m.deps.ClaudeRetention == nil {
		return claude.Retention{}
	}
	return m.deps.ClaudeRetention(workDir)
}

// ClaudeProbeConfig constructs the canonical account probe configuration.
func (m *Manager) ClaudeProbeConfig(binary string, pins map[string]string) claude.ProbeConfig {
	return m.claudeProbeConfig(binary, pins)
}

// ClaudeAccountProbe is a canonical Claude account probe of accountID: the
// cache key its answer is filed under and the configuration it runs with,
// built from one read of the custom environment. A probe that waits for the
// reconcile lock through an edit of that environment runs, and files its
// answer, under the one it was started with.
func (m *Manager) ClaudeAccountProbe(binary, accountID string) (provider.ProbeCacheKey, claude.ProbeConfig) {
	custom := m.providerCustomEnv(string(provider.Claude))
	return providerProbeCacheKeyForAccountEnv(binary, accountID, custom), m.claudeProbeConfigWith(binary, custom, nil)
}

// claudeProbeCredentialReader hands the probe a read-only view of the
// credential its CLI will authenticate with, so the probe can hold the process
// open until a token rotation it triggered is durable rather than killing the
// CLI mid-refresh — see claude.ProbeConfig.ReadCredential.
//
// It follows the same home the Env pins do, because that is the home whose
// credential the CLI reads and rotates: an ephemeral probe home's rotation is
// exactly as unrecoverable as the canonical one's, and its slot is the only
// holder of that chain.
//
// nil when there is no credential store to read — a state in which no probe
// can run at all, so there is nothing to protect.
//
// An absent credential reads as fs.ErrNotExist, the probe's signal for
// "nothing to rotate". The macOS Keychain reports absence as
// ErrCredentialMissing, which the probe would otherwise treat as unreadable
// and hold every CLI open for the full rotation settle window.
func (m *Manager) claudeProbeCredentialReader(pins map[string]string) func() ([]byte, error) {
	credentials := m.credentials
	if credentials == nil {
		return nil
	}
	read := func() (provideraccounts.CredentialSnapshot, error) {
		return credentials.ReadCredentialSnapshot(string(provider.Claude), "", true)
	}
	if home := pins["CLAUDE_CONFIG_DIR"]; home != "" {
		read = func() (provideraccounts.CredentialSnapshot, error) {
			return credentials.ReadCredentialAtHome(string(provider.Claude), home)
		}
	}
	return func() ([]byte, error) {
		snapshot, err := read()
		if provideraccounts.IsCredentialMissing(err) {
			return nil, fs.ErrNotExist
		}
		return snapshot.Bytes(), err
	}
}

// codexProbeConfig is the Codex counterpart to claudeProbeConfig.
func (m *Manager) codexProbeConfig(binary string, pins map[string]string) codex.ProbeConfig {
	return m.codexProbeConfigWith(binary, m.providerCustomEnv(string(provider.Codex)), pins)
}

// codexProbeConfigWith is codexProbeConfig for a custom environment already
// read.
func (m *Manager) codexProbeConfigWith(binary string, custom, pins map[string]string) codex.ProbeConfig {
	return codex.ProbeConfig{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Env:     probeEnvWith(custom, pins),
		Reaper:  m.deps.Reaper,
	}
}

// CodexProbeConfig constructs the canonical account probe configuration.
func (m *Manager) CodexProbeConfig(binary string, pins map[string]string) codex.ProbeConfig {
	return m.codexProbeConfig(binary, pins)
}

// CodexAccountProbe is ClaudeAccountProbe for Codex.
func (m *Manager) CodexAccountProbe(binary, accountID string) (provider.ProbeCacheKey, codex.ProbeConfig) {
	custom := m.providerCustomEnv(string(provider.Codex))
	return providerProbeCacheKeyForAccountEnv(binary, accountID, custom), m.codexProbeConfigWith(binary, custom, nil)
}

// AgyCanonicalHome is an agy.HomeSource: the home every agy process for the
// canonical login runs against, on the store this manager reads and writes
// that login in, decided as each launch starts because a login outside the
// app can add or remove the Keychain item at any time. The home is agy's
// default under the process HOME when that is the directory this manager
// reads, and that directory named otherwise (an isolated boot keeping the
// person's HOME for its children).
func (m *Manager) AgyCanonicalHome(ctx context.Context) (agy.Home, error) {
	if m.credentials == nil {
		return agy.Home{}, errors.New("provider account storage is unavailable")
	}
	paths, err := m.credentials.Paths(string(provider.Agy))
	if err != nil {
		return agy.Home{}, err
	}
	fileStorage, err := m.credentials.AgyCanonicalFileStorage(ctx)
	if err != nil {
		return agy.Home{}, fmt.Errorf("choose the Antigravity login store: %w", err)
	}
	dir := paths.SharedHome
	// agy resolves its default home under the process HOME only when that is
	// an absolute path; otherwise the directory is named, and a relative one
	// is refused (agy.CanonicalHome).
	if processHome, err := os.UserHomeDir(); err == nil && filepath.IsAbs(processHome) && filepath.Clean(dir) == filepath.Join(processHome, ".gemini", agy.DefaultAppDataName) {
		dir = ""
	}
	return agy.CanonicalHome(dir, fileStorage)
}

func (m *Manager) agyProbeConfig(binary string, home agy.HomeSource, custom map[string]string) agy.ProbeConfig {
	return agy.ProbeConfig{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Home:    home,
		Env:     probeEnvWith(custom, nil),
		Reaper:  m.deps.Reaper,
	}
}

// probeCanonicalAgy runs the Antigravity account probe against the canonical
// home, decided inside the probe's time limit.
func (m *Manager) probeCanonicalAgy(ctx context.Context, binary string, custom map[string]string) (provider.AccountInfo, error) {
	return agy.ProbeAccount(ctx, m.agyProbeConfig(binary, m.AgyCanonicalHome, custom))
}

// AgyAccountProbe is ClaudeAccountProbe for Antigravity: the cache key and
// the canonical probe, from one read of settings.
func (m *Manager) AgyAccountProbe(binary, accountID string) (provider.ProbeCacheKey, func(context.Context) (provider.AccountInfo, error)) {
	custom := m.providerCustomEnv(string(provider.Agy))
	return providerProbeCacheKeyForAccountEnv(binary, accountID, custom), func(ctx context.Context) (provider.AccountInfo, error) {
		return m.probeCanonicalAgy(ctx, binary, custom)
	}
}

// ProbeWorkDir is Claude's user scope and the parent for isolated Codex account directories.
func ProbeWorkDir() string { return providerProbeWorkDir() }

// providerProbeWorkDir selects Claude's working directory and the parent
// directory for isolated Codex account processes.
//
// Account probes ask a global question — "which login does this CLI hold?"
// — and three of their consumers depend on the answer describing the
// canonical native home: managed-account adoption, external-login
// reconciliation, and probeSelectedClaudeRateLimits, which leans on the
// probe to drive Claude's own OAuth refresh-token rotation. Running them in
// a project directory would let a project-scoped `.claude/settings.json`
// env block (CLAUDE_CODE_USE_BEDROCK, say) answer for the whole app: the
// identity would belong to one workspace, and the rotation this path exists
// to trigger would silently not happen at all.
//
// Before this was pinned, the probe inherited the app process's cwd —
// Finder's default in one install, a Bedrock repo in another, with the
// result cached process-wide either way. The user home directory supplies Claude
// user scope and a stable parent for Codex account working directories. Codex
// creates an empty child directory and disables ancestor config discovery so
// a temporary CODEX_HOME cannot turn ~/.codex into a second project layer.
//
// Making the probe reflect the active thread's workspace instead would be a
// product change, not a bug fix — the account identity would then flip as
// the user switches projects. See t3-improvements.md §2.3.
//
// os.TempDir is the fallback for a host with no resolvable home. It is not
// as stable (some platforms clear it between boots) but it is absolute and
// project-free, which is what the probe contract requires.
func providerProbeWorkDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fallback := os.TempDir()
		log.Printf("provider probe: no home directory (%v); running probes in %s", err, fallback)
		return fallback
	}
	return home
}
