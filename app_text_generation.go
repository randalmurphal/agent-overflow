package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/textgen"
)

// resolveTextGenerationConfig assembles the textgen.Config for the
// caller's chosen provider. The CLI executor seam (a.textGenerationExecutor)
// is wired here so production calls go through textgen.ExecCLI while tests
// can inject a fake.
//
// Layer 1 fallback: if the configured provider's binary isn't on PATH, the
// alternate provider is substituted (with ITS own default model — never the
// user's TextGenerationModel setting, which likely names a model the
// alternate doesn't support). Returns the user's preference unchanged when
// neither binary resolves so the caller surfaces a coherent
// "binary not found" error.
func (a *App) resolveTextGenerationConfig() textgen.Config {
	s := a.currentSettings()
	preferred := strings.TrimSpace(s.TextGenerationProvider)
	if preferred == "" {
		preferred = settings.DefaultSettings.TextGenerationProvider
	}

	claudeBin := a.providerBinaryPath(string(provider.Claude))
	codexBin := a.providerBinaryPath(string(provider.Codex))
	chosen := textgen.PickAvailableProvider(preferred, claudeBin, codexBin, a.resolveLookPath())

	// Effort is set from settings regardless of provider — both Codex's
	// `--config model_reasoning_effort=...` and Claude commit-message's
	// `--effort` flag consume it. (Claude's thread-title path ignores it.)
	effort := strings.TrimSpace(s.TextGenerationReasoningEffort)
	if effort == "" {
		effort = settings.DefaultSettings.TextGenerationReasoningEffort
	}

	cfg := textgen.Config{
		Provider: chosen,
		Effort:   effort,
		Exec:     a.resolveTextGenerationExecutor(),
	}

	switch chosen {
	case string(provider.Codex):
		cfg.Binary = codexBin
		// User's TextGenerationModel only applies when we kept the
		// configured provider. After a substitution, fall through to
		// DefaultCodexModel — a Claude model slug would fail here.
		if chosen == preferred {
			cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		}
		if cfg.Model == "" {
			cfg.Model = textgen.DefaultCodexModel
		}
	case string(provider.Claude):
		cfg.Binary = claudeBin
		if chosen == preferred {
			cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		}
		if cfg.Model == "" {
			cfg.Model = textgen.DefaultClaudeModel
		}
	}

	return cfg
}

// resolveTextGenerationConfigFor builds a textgen.Config for an explicit
// provider name, used by the Layer 2 retry path to construct the alternate
// provider's config without re-running PickAvailableProvider. Returns ok=false
// when the alternate's binary doesn't resolve on PATH — the caller must NOT
// attempt the run in that case.
//
// Always uses the provider's default model. The user's TextGenerationModel
// is deliberately ignored because cross-provider model carry-over is
// guaranteed to fail at the CLI boundary.
func (a *App) resolveTextGenerationConfigFor(providerName string) (textgen.Config, bool) {
	lookPath := a.resolveLookPath()
	effort := strings.TrimSpace(a.currentSettings().TextGenerationReasoningEffort)
	if effort == "" {
		effort = settings.DefaultSettings.TextGenerationReasoningEffort
	}
	cfg := textgen.Config{
		Provider: providerName,
		Effort:   effort,
		Exec:     a.resolveTextGenerationExecutor(),
	}
	switch providerName {
	case string(provider.Codex):
		cfg.Binary = a.providerBinaryPath(string(provider.Codex))
		if cfg.Binary == "" || lookPath(cfg.Binary) != nil {
			return textgen.Config{}, false
		}
		cfg.Model = textgen.DefaultCodexModel
	case string(provider.Claude):
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
		if cfg.Binary == "" || lookPath(cfg.Binary) != nil {
			return textgen.Config{}, false
		}
		cfg.Model = textgen.DefaultClaudeModel
	default:
		return textgen.Config{}, false
	}
	return cfg, true
}

// availableTextGenerationProviders probes the configured provider
// binaries and returns the names whose binaries resolve on PATH. Used
// for chat-model seeding so a Claude-only or Codex-only environment
// gets a default that actually works rather than a hardcoded Claude
// preference that silently fails.
func (a *App) availableTextGenerationProviders() []string {
	lookPath := a.resolveLookPath()
	var available []string
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		bin := a.providerBinaryPath(name)
		if bin != "" && lookPath(bin) == nil {
			available = append(available, name)
		}
	}
	return available
}

// runTextGenWithFallback executes runOnce against primary; on any non-
// context-canceled error retries once with the alternate provider when
// its binary resolves and the shared deadline hasn't elapsed. Both
// attempts must use the SAME deadline (managed by the caller's
// derived contexts inside runOnce) so the total wall-clock budget
// stays bounded regardless of how many providers we try.
//
// When both attempts fail, the PRIMARY error is surfaced — the user-
// visible signal should be about the configured provider rather than
// the silent fallback path that the user didn't ask for. This keeps
// the Settings UI's red status pill and the returned error message
// pointing at the same root cause.
func runTextGenWithFallback[T any](
	a *App,
	primary textgen.Config,
	deadline time.Time,
	runOnce func(cfg textgen.Config) (T, error),
) (T, error) {
	result, err := runOnce(primary)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		// App shutdown or user navigated away — don't retry.
		return result, err
	}
	altName := otherProvider(primary.Provider)
	if altName == "" || !time.Now().Before(deadline) {
		return result, err
	}
	altCfg, ok := a.resolveTextGenerationConfigFor(altName)
	if !ok {
		return result, err
	}
	altResult, altErr := runOnce(altCfg)
	if altErr != nil {
		return result, err
	}
	return altResult, nil
}

// remainingBudget returns the time left before ctx's deadline, clamped to
// `fallback` when ctx has no deadline (the test seam path bypasses
// context.WithDeadline entirely) and to zero when the deadline has
// already elapsed. Used to format honest "timed out after X" errors in
// the Layer 2 retry path: the per-attempt budget is the remaining
// shared deadline, not the full per-task constant.
func remainingBudget(ctx context.Context, fallback time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fallback
	}
	remaining := time.Until(deadline)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// otherProvider returns the two-provider universe's complement. Trivial,
// but lives here so the call sites read like English.
func otherProvider(name string) string {
	switch name {
	case string(provider.Claude):
		return string(provider.Codex)
	case string(provider.Codex):
		return string(provider.Claude)
	default:
		return ""
	}
}

// resolveLookPath returns the lookPath callback used for provider
// availability detection. Tests override a.lookPathFn; production falls
// through to exec.LookPath (discarding the resolved path — the helpers
// only care whether resolution succeeded).
func (a *App) resolveLookPath() func(string) error {
	if a.lookPathFn != nil {
		return a.lookPathFn
	}
	return func(bin string) error {
		_, err := exec.LookPath(bin)
		return err
	}
}

// resolveTextGenerationExecutor returns the executor used by both the
// primary and the Layer 2 fallback path. Production callers leave
// textGenerationExecutor nil and the executor falls through to
// textgen.ExecCLI; tests install a fake.
func (a *App) resolveTextGenerationExecutor() textgen.CLIExecutor {
	if a.textGenerationExecutor != nil {
		return a.textGenerationExecutor
	}
	return textgen.ExecCLI
}
