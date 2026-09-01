package provideraccountapp

import (
	"log"
	"maps"
	"os"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
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
	custom := m.providerCustomEnv(providerName)
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
	return claude.ProbeConfig{
		Binary:         binary,
		WorkDir:        providerProbeWorkDir(),
		Env:            m.providerProbeEnv(string(provider.Claude), pins),
		ReadCredential: m.claudeProbeCredentialReader(pins),
	}
}

// ClaudeProbeConfig constructs the canonical account probe configuration.
func (m *Manager) ClaudeProbeConfig(binary string, pins map[string]string) claude.ProbeConfig {
	return m.claudeProbeConfig(binary, pins)
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
func (m *Manager) claudeProbeCredentialReader(pins map[string]string) func() ([]byte, error) {
	credentials := m.credentials
	if credentials == nil {
		return nil
	}
	if home := pins["CLAUDE_CONFIG_DIR"]; home != "" {
		return func() ([]byte, error) {
			snapshot, err := credentials.ReadCredentialAtHome(string(provider.Claude), home)
			return snapshot.Data, err
		}
	}
	return func() ([]byte, error) {
		snapshot, err := credentials.ReadCredentialSnapshot(string(provider.Claude), "", true)
		return snapshot.Data, err
	}
}

// codexProbeConfig is the Codex counterpart to claudeProbeConfig.
func (m *Manager) codexProbeConfig(binary string, pins map[string]string) codex.ProbeConfig {
	return codex.ProbeConfig{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Env:     m.providerProbeEnv(string(provider.Codex), pins),
	}
}

// CodexProbeConfig constructs the canonical account probe configuration.
func (m *Manager) CodexProbeConfig(binary string, pins map[string]string) codex.ProbeConfig {
	return m.codexProbeConfig(binary, pins)
}

// ProbeWorkDir is the stable project-free directory used by account probes.
func ProbeWorkDir() string { return providerProbeWorkDir() }

// providerProbeWorkDir is the directory every account probe runs in.
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
// result cached process-wide either way. The user home directory is the
// deliberate replacement: it always exists, it is identical across launches,
// and it holds no project scope a probe could pick up (`~/.claude/` is the
// USER settings scope, which both CLIs read from any cwd).
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
