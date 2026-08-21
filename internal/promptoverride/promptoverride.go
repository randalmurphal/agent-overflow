package promptoverride

import (
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/settings"
)

// The placeholder vocabulary. This list is CLOSED: Render substitutes
// these tokens and nothing else, so a typo (`{{WORKDIRR}}`) survives into
// the prompt verbatim where the user can see it, rather than failing the
// spawn or silently vanishing.
const (
	TokenWorkDir   = "{{WORKDIR}}"
	TokenIsGitRepo = "{{IS_GIT_REPO}}"
	TokenGitBlock  = "{{GIT_BLOCK}}"
	TokenPlatform  = "{{PLATFORM}}"
	TokenOSVersion = "{{OS_VERSION}}"
	TokenModelName = "{{MODEL_NAME}}"
	TokenModelID   = "{{MODEL_ID}}"
	TokenMemoryDir = "{{MEMORY_DIR}}"
)

// Facts are the spawn-time values the placeholders render to. Every field
// is a plain string: an absent fact renders as empty, never as the raw
// token, because a prompt that still shows `{{GIT_BLOCK}}` to the model is
// worse than one with a blank section.
type Facts struct {
	// WorkDir is the workspace path the session will actually run in —
	// the resolved one, including any per-feature override.
	WorkDir string
	// IsGitRepo is rendered text, not a bool, so the prompt reads the way
	// Claude's own Environment block does ("Yes" / "No").
	IsGitRepo string
	// GitBlock is a compact branch / status / recent-commits snapshot.
	// Empty outside a repository and whenever the probe failed.
	GitBlock string
	// Platform is the Go GOOS token (linux, darwin, windows).
	Platform  string
	OSVersion string
	// ModelName is the catalog display name; ModelID is the id AO launches
	// the session with.
	ModelName string
	ModelID   string
	// MemoryDir is the Claude memory directory for this workspace. Empty
	// for Codex and whenever the directory could not be resolved.
	MemoryDir string
}

// Match returns the first ENABLED entry whose Models contains the
// session's model. Both sides are normalized through
// provider.NormalizeModelSlug — which is also where the Claude context-tier
// marker is trimmed, so a selection stored as `claude-opus-5` still matches
// a thread running the 1M spelling `claude-opus-5[1m]`: the tier is a
// context window, not a model identity.
//
// Order is the user's: entries are evaluated as listed, first match wins.
func Match(entries []settings.PromptOverride, providerName, model string) (settings.PromptOverride, bool) {
	want := normalizeSlug(providerName, model)
	if want == "" {
		return settings.PromptOverride{}, false
	}
	for _, entry := range entries {
		if !entry.Enabled || strings.TrimSpace(entry.Prompt) == "" {
			continue
		}
		for _, candidate := range entry.Models {
			if normalizeSlug(providerName, candidate) == want {
				return entry, true
			}
		}
	}
	return settings.PromptOverride{}, false
}

// normalizeSlug is provider.NormalizeModelSlug plus a blank guard. The
// context-marker trim is NOT applied on top: NormalizeModelSlug already owns
// that rule for claude / claude-tui, and applying it to codex here would
// enforce a Claude-only spelling the provider package deliberately leaves
// alone — a bracketed codex id would be trimmed on this path and nowhere
// else, so an entry saved through the picker could stop matching its own
// thread.
func normalizeSlug(providerName, model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	return provider.NormalizeModelSlug(providerName, trimmed)
}

// Uses reports whether a prompt references a placeholder. Callers use it
// to decide whether a fact is worth computing at all — the git probe and
// the memory-directory mkdir are both side-effecting or subprocess-backed,
// and a prompt that never mentions them must not pay for them.
func Uses(prompt, token string) bool {
	return strings.Contains(prompt, token)
}

// Render substitutes the known placeholders in one left-to-right pass. A
// substituted value is never rescanned, so a fact that happens to contain
// a token (a workspace path with braces in it) cannot expand again.
func Render(prompt string, facts Facts) string {
	return strings.NewReplacer(
		TokenWorkDir, facts.WorkDir,
		TokenIsGitRepo, facts.IsGitRepo,
		TokenGitBlock, facts.GitBlock,
		TokenPlatform, facts.Platform,
		TokenOSVersion, facts.OSVersion,
		TokenModelName, facts.ModelName,
		TokenModelID, facts.ModelID,
		TokenMemoryDir, facts.MemoryDir,
	).Replace(prompt)
}

// ClaudeMemoryDir returns `<home>/.claude/projects/<slug>/memory` for a
// workspace — the directory Claude Code's memory feature reads and writes,
// and the one the CLI stops creating under `--system-prompt`.
//
// The slug is NOT computed here. It comes from
// sessionfork.WorkspaceProjectDir, which is the same encoder the session
// relocation writer uses, so the directory AO creates and the one a resumed
// CLI reads cannot drift apart. Every workspace resolves, over-length paths
// included (the CLI's truncate-and-hash slug is reproduced exactly), so the
// error is the only failure answer: the workspace path could not be
// canonicalized, which means it does not exist.
func ClaudeMemoryDir(home, workDir string) (string, error) {
	projectDir, err := sessionfork.WorkspaceProjectDir(
		filepath.Join(home, ".claude", "projects"),
		workDir,
	)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectDir, "memory"), nil
}
