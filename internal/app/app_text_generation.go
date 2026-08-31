package app

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"strings"
	"testing"
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

	// Custom environment follows the provider actually chosen, not the
	// preferred one: after a substitution the run talks to the other backend,
	// and carrying the first provider's endpoint across would point it at a
	// gateway that has never heard of it.
	cfg.Env = a.providerCustomEnv(chosen)

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

	coerceTextGenerationEffort(&cfg)
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
		Env:      a.providerCustomEnv(providerName),
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
	coerceTextGenerationEffort(&cfg)
	return cfg, true
}

// coerceTextGenerationEffort resolves the effort a one-shot generation run
// passes to the CLI. Unlike a thread's effort this value is never persisted, so
// it can express "this model takes no effort at all" — which the CLI runners
// render as an omitted flag rather than an invented tier. Coercing a model
// without tiers up to the provider default would raise cost silently, on the
// one surface the user never sees a control for.
func coerceTextGenerationEffort(cfg *textgen.Config) {
	if provider.ModelDeclaresNoReasoningEffort(cfg.Provider, cfg.Model) {
		cfg.Effort = ""
		return
	}
	cfg.Effort = string(provider.CoerceReasoningEffortForModel(
		cfg.Provider,
		cfg.Model,
		provider.NormalizeReasoningEffort(cfg.Effort),
	))
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
// its binary resolves.
//
// Each attempt gets its OWN full budget, handed to runOnce as a fresh
// deadline. A shared deadline used to bound both attempts together,
// which meant a primary that failed by TIMING OUT left the fallback no
// time at all — the 2026-08-16 incident, where a usage-limited Codex
// was followed by a Claude leg killed on arrival. context.DeadlineExceeded
// therefore must NOT short-circuit the retry; only context.Canceled
// (app shutdown / caller gone) does.
//
// When both attempts fail, the PRIMARY error is surfaced — the user-
// visible signal should be about the configured provider rather than
// the silent fallback path that the user didn't ask for. This keeps
// the Settings UI's red status pill and the returned error message
// pointing at the same root cause. The alternate's failure is logged
// rather than discarded: it is the only record that the fallback ran.
func runTextGenWithFallback[T any](
	a *App,
	primary textgen.Config,
	budget time.Duration,
	runOnce func(cfg textgen.Config, deadline time.Time) (T, error),
) (T, error) {
	result, err := runOnce(primary, time.Now().Add(budget))
	if err == nil {
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		// App shutdown or user navigated away — don't retry.
		return result, err
	}
	if a.lifeCtx().Err() != nil {
		// The app is going down. The primary's error may not be spelled
		// context.Canceled (a killed CLI reports its own failure), and
		// spawning a second provider CLI into a teardown is exactly the
		// shape that leaves orphans behind.
		return result, err
	}
	altName := otherProvider(primary.Provider)
	if altName == "" {
		return result, err
	}
	altCfg, ok := a.resolveTextGenerationConfigFor(altName)
	if !ok {
		return result, err
	}
	altResult, altErr := runOnce(altCfg, time.Now().Add(budget))
	if altErr != nil {
		// No budget in the line: a timeout error from textgen already
		// names the CLI and the duration it was given.
		log.Printf(
			"textgen: fallback %s failed after primary %s error: %s",
			altCfg.Provider, primary.Provider, textgen.RedactError(altErr),
		)
		return result, err
	}
	return altResult, nil
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
//
// Inside a test binary the fallthrough is refused outright: textgen runs a
// real provider CLI, usually from a detached goroutine (thread titles,
// commit messages) that outlives its test, against whatever HOME the
// fixture left in place — the exact shape of incident 2026-08-03. A fixture
// that wants textgen behavior installs a fake; testing.Testing() is false
// in every production process, so this branch costs ordinary runs nothing.
func (a *App) resolveTextGenerationExecutor() textgen.CLIExecutor {
	if a.textGenerationExecutor != nil {
		return a.textGenerationExecutor
	}
	if testing.Testing() {
		return func(context.Context, textgen.CLISpec) (textgen.CLIResult, error) {
			return textgen.CLIResult{}, errors.New(
				"tests must not run a real provider CLI through textgen; assign app.textGenerationExecutor a fake",
			)
		}
	}
	return textgen.ExecCLI
}
