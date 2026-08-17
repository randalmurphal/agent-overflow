package main

import (
	"log"
	"os"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

type providerAccountSelection struct {
	Generation uint64
	AccountID  string
	Account    provider.AccountInfo
}

func (a *App) captureProviderAccountSelection(providerName string) providerAccountSelection {
	a.providerAccountMu.RLock()
	defer a.providerAccountMu.RUnlock()
	return a.providerAccountSelectionLocked(providerName)
}

func (a *App) providerAccountSelectionLocked(providerName string) providerAccountSelection {
	if a.providerAccounts == nil {
		return providerAccountSelection{}
	}
	selection := providerAccountSelection{
		Generation: a.providerAccounts.Generation(providerName),
	}
	account, ok := a.providerAccounts.Active(providerName, time.Now())
	if !ok {
		return selection
	}
	selection.AccountID = account.ID
	selection.Account = providerAccountInfo(account)
	return selection
}

func (a *App) providerProbeCacheKey(providerName, binary string) provider.ProbeCacheKey {
	accountID := ""
	if a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
			accountID = account.ID
		}
	}
	return a.providerProbeCacheKeyForAccount(providerName, binary, accountID)
}

func (a *App) providerProbeCacheKeyForAccount(providerName, binary, accountID string) provider.ProbeCacheKey {
	return providerProbeCacheKeyForAccountEnv(binary, accountID, a.providerCustomEnv(providerName))
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

// providerCustomEnv is the user's configured environment for a provider, as
// the override map every spawn path consumes. nil when nothing is configured.
//
// claude-tui resolves to Claude's list (one binary, one backend); unknown
// providers get nil.
func (a *App) providerCustomEnv(providerName string) map[string]string {
	return a.currentSettings().ProviderEnvMap(providerName)
}

// providerProbeEnv merges the user's custom environment with the pins a
// specific probe needs (a temporary CLAUDE_CONFIG_DIR / CODEX_HOME). Pins win:
// they name the credential home the probe exists to interrogate, and the
// settings layer already refuses to let a user configure those names.
//
// Returns nil when both sides are empty so probes keep their "inherit the
// process environment" fast path.
func (a *App) providerProbeEnv(providerName string, pins map[string]string) map[string]string {
	custom := a.providerCustomEnv(providerName)
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

// claudeProbeConfig is the ONE constructor for a Claude probe invocation:
// binary, the pinned probe workdir, and the user's custom environment merged
// under whatever config-home pin the caller needs. Callers that need a
// non-default timeout or an OnSnapshot hook set those on the returned value.
//
// Constructing ProbeConfig literals at call sites is what let the custom
// environment reach the session but not the probe — which is precisely the
// failure this feature must not have, because an ANTHROPIC_BASE_URL that
// changes which backend answers must change who the probe reports.
func (a *App) claudeProbeConfig(binary string, pins map[string]string) claude.ProbeConfig {
	return claude.ProbeConfig{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Env:     a.providerProbeEnv(string(provider.Claude), pins),
	}
}

// codexProbeConfig is the Codex counterpart to claudeProbeConfig.
func (a *App) codexProbeConfig(binary string, pins map[string]string) codex.ProbeConfig {
	return codex.ProbeConfig{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Env:     a.providerProbeEnv(string(provider.Codex), pins),
	}
}

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
