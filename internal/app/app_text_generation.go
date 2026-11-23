package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccountapp"
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
//
// The error says the provider lists no model of its text-generation family,
// or that Claude has not reported its models (textGenerationDefaultModel);
// the config still names the provider, so the Layer 2 retry can turn to the
// other one. ctx cancels that wait.
func (a *App) resolveTextGenerationConfig(ctx context.Context) (textgen.Config, error) {
	s := a.currentSettings()
	preferred := strings.TrimSpace(s.TextGenerationProvider)
	if preferred == "" {
		preferred = settings.DefaultSettings.TextGenerationProvider
	}

	claudeBin := a.providerBinaryPath(string(provider.Claude))
	codexBin := a.providerBinaryPath(string(provider.Codex))
	agyBin := a.providerBinaryPath(string(provider.Agy))
	lookPath := a.resolveLookPath()
	chosen := textgen.PickAvailableProviderOrdered(
		preferred,
		map[string]string{
			string(provider.Claude): claudeBin,
			string(provider.Codex):  codexBin,
			string(provider.Agy):    agyBin,
		},
		textGenerationFallbackOrder(preferred),
		lookPath,
	)

	// Effort is set from settings regardless of provider — Codex's
	// `--config model_reasoning_effort=...`, Claude's `--effort`, and Agy's
	// `--effort` flag consume it.
	effort := strings.TrimSpace(s.TextGenerationReasoningEffort)
	if effort == "" {
		effort = settings.DefaultSettings.TextGenerationReasoningEffort
	}

	cfg := textgen.Config{
		Provider: chosen,
		Effort:   effort,
		Admit:    a.textGenerationAdmission(chosen),
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
	case string(provider.Claude):
		cfg.Binary = claudeBin
		cfg.ClaudeRetention = a.claudeRetention
	case string(provider.Agy):
		cfg.Binary = agyBin
		cfg.Env = a.agyTextGenEnv(cfg.Env)
	}
	// User's TextGenerationModel only applies when we kept the configured
	// provider. After a substitution the provider's own default runs: the
	// other provider's model slug would fail here.
	if chosen == preferred {
		cfg.Model = strings.TrimSpace(s.TextGenerationModel)
	}
	// Neither binary resolves: the run fails on the binary, which is the
	// error to surface, so no model list is read for it.
	model, known := provider.ModelInfo{Slug: cfg.Model}, false
	if cfg.Model == "" && cfg.Binary != "" && lookPath(cfg.Binary) == nil {
		var err error
		if model, err = a.textGenerationDefaultModel(ctx, chosen); err != nil {
			return cfg, err
		}
		cfg.Model, known = model.Slug, true
	}
	cfg.Effort = textGenerationEffort(cfg.Provider, cfg.Effort, model, known)
	return cfg, nil
}

// resolveTextGenerationConfigFor builds a textgen.Config for an explicit
// provider name, used by the Layer 2 retry path to construct the alternate
// provider's config without re-running PickAvailableProvider. Returns ok=false
// when the alternate's binary doesn't resolve on PATH, or when it lists no
// model of its text-generation family, which is logged — the caller must
// NOT attempt the run in that case.
//
// Always uses the provider's default model. The user's TextGenerationModel
// is deliberately ignored because cross-provider model carry-over is
// guaranteed to fail at the CLI boundary.
func (a *App) resolveTextGenerationConfigFor(ctx context.Context, providerName string) (textgen.Config, bool) {
	lookPath := a.resolveLookPath()
	effort := strings.TrimSpace(a.currentSettings().TextGenerationReasoningEffort)
	if effort == "" {
		effort = settings.DefaultSettings.TextGenerationReasoningEffort
	}
	cfg := textgen.Config{
		Provider: providerName,
		Effort:   effort,
		Env:      a.providerCustomEnv(providerName),
		Admit:    a.textGenerationAdmission(providerName),
		Exec:     a.resolveTextGenerationExecutor(),
	}
	switch providerName {
	case string(provider.Codex):
		cfg.Binary = a.providerBinaryPath(string(provider.Codex))
		if cfg.Binary == "" || lookPath(cfg.Binary) != nil {
			return textgen.Config{}, false
		}
	case string(provider.Claude):
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
		if cfg.Binary == "" || lookPath(cfg.Binary) != nil {
			return textgen.Config{}, false
		}
		cfg.ClaudeRetention = a.claudeRetention
	case string(provider.Agy):
		cfg.Binary = a.providerBinaryPath(string(provider.Agy))
		if cfg.Binary == "" || lookPath(cfg.Binary) != nil {
			return textgen.Config{}, false
		}
		cfg.Model = textgen.DefaultAgyModel
		cfg.Env = a.agyTextGenEnv(cfg.Env)
	default:
		return textgen.Config{}, false
	}
	model, err := a.textGenerationDefaultModel(ctx, providerName)
	if err != nil {
		log.Printf("textgen: fallback %s skipped: %v", providerName, err)
		return textgen.Config{}, false
	}
	cfg.Model = model.Slug
	cfg.Effort = textGenerationEffort(cfg.Provider, cfg.Effort, model, true)
	return cfg, true
}

func (a *App) agyTextGenEnv(base map[string]string) map[string]string {
	if !a.hasManagedAgyCredentialFile() {
		return base
	}
	out := make(map[string]string)
	for k, v := range base {
		out[k] = v
	}
	out["AGY_ACP_FORCE_FILE_STORAGE"] = "1"
	out["SSH_CONNECTION"] = "127.0.0.1 0 127.0.0.1 0"
	return out
}

// textGenerationDefaultModel is the catalog row of the model a run on
// providerName takes when settings leave the model blank: the newest model
// of the provider's text-generation family this computer lists
// (provider.ResolveModel), the rule agent profiles follow. Claude's list is
// its CLI's report, which it waits for (awaitClaudeModels). A provider that
// lists no model of the family is an error naming what it lists. The row
// comes from the same catalog read the choice did, so the run's effort is
// decided by the model that runs.
func (a *App) textGenerationDefaultModel(ctx context.Context, providerName string) (provider.ModelInfo, error) {
	family, ok := settings.TextGenerationDefaultFamily(providerName)
	if !ok {
		return provider.ModelInfo{}, fmt.Errorf("text generation does not run on %s", providerName)
	}
	var catalog provider.ModelCatalog
	var err error
	if providerName == string(provider.Claude) {
		catalog, err = a.awaitClaudeModels(ctx)
	} else {
		catalog, err = a.modelCatalogContext(ctx, providerName)
	}
	if err != nil {
		return provider.ModelInfo{}, fmt.Errorf("text generation: %s model list: %w", providerName, err)
	}
	var listed, kept []string
	for _, model := range catalog.Models {
		if model.ProviderUnlisted {
			kept = append(kept, model.Slug)
		} else {
			listed = append(listed, model.Slug)
		}
	}
	slug, ok := provider.ResolveModel(provider.ParseModelRef(providerName, family), listed, kept)
	if !ok {
		return provider.ModelInfo{}, fmt.Errorf("text generation: %s lists no %s model; it lists %s", providerName, family, strings.Join(listed, ", "))
	}
	index := slices.IndexFunc(catalog.Models, func(model provider.ModelInfo) bool { return model.Slug == slug })
	if index < 0 {
		return provider.ModelInfo{}, fmt.Errorf("text generation: %s's model list has no row for %s", providerName, slug)
	}
	return catalog.Models[index], nil
}

// awaitClaudeModels is Claude's catalog once the CLI of the account in use
// has reported its models, live or restored from that account's saved
// record (claudemodels.BackedByIdentity). Until then the list is the
// shipped one, which names models the CLI may not run, or the one another
// account on the binary reported, which this account may not run. So it
// waits for the report (claudecatalog.ModelsReported) as long as the probe
// that makes it may take (claude.DefaultProbeTimeout), and past that fails
// saying Claude has not reported. ctx and the app's shutdown end the wait.
func (a *App) awaitClaudeModels(ctx context.Context) (provider.ModelCatalog, error) {
	bound := cmp.Or(a.claudeModelsWaitForTest, claude.DefaultProbeTimeout)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		reported := claudecatalog.ModelsReported()
		catalog, backing := a.providerDiscoveryService().ClaudeAccountCatalog(string(provider.Claude))
		if backing == claudemodels.BackedByIdentity {
			a.unreadableModels.report(string(provider.Claude), catalog.Models)
			return catalog, nil
		}
		select {
		case <-reported:
		case <-ctx.Done():
			return provider.ModelCatalog{}, fmt.Errorf("waiting for Claude to report its models: %w", ctx.Err())
		case <-a.lifeCtx().Done():
			return provider.ModelCatalog{}, fmt.Errorf("waiting for Claude to report its models: %w", a.lifeCtx().Err())
		case <-timer.C:
			return provider.ModelCatalog{}, fmt.Errorf("no answer in %s: Claude has not reported its models", bound)
		}
	}
}

// textGenerationEffort is the effort a one-shot generation run passes to the
// CLI for model, given its row when known. Unlike a thread's effort this value
// is never persisted, so it can express "this model takes no effort at all",
// which the CLI runners render as an omitted flag rather than an invented
// tier. Coercing a model without tiers up to the provider default would raise
// cost silently, on the one surface the user never sees a control for. A
// model named in settings has no row from a catalog read; the shipped table
// answers for it.
func textGenerationEffort(providerName, effort string, model provider.ModelInfo, known bool) string {
	if !known {
		model, known = provider.FindModel(providerName, model.Slug)
	}
	if known {
		if len(model.ReasoningEfforts) == 0 {
			return ""
		}
		return string(provider.CoerceReasoningEffortForModelInfo(model, provider.NormalizeReasoningEffort(effort)))
	}
	return string(provider.CoerceReasoningEffortForModel(providerName, model.Slug, provider.NormalizeReasoningEffort(effort)))
}

// availableTextGenerationProviders probes the configured provider
// binaries and returns the names whose binaries resolve on PATH. Used
// for chat-model seeding so a Claude-only or Codex-only environment
// gets a default that actually works rather than a hardcoded Claude
// preference that silently fails.
func (a *App) availableTextGenerationProviders() []string {
	lookPath := a.resolveLookPath()
	var available []string
	for _, kind := range provider.StandaloneProviders() {
		name := string(kind)
		bin := a.providerBinaryPath(name)
		if bin != "" && lookPath(bin) == nil {
			available = append(available, name)
		}
	}
	return available
}

// runTextGenWithFallback executes runOnce against the configured provider
// (resolveTextGenerationConfig); on any non-context-canceled error, a
// config that could not name a model among them, retries once with the
// alternate provider when its binary resolves.
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
//
// ctx is the caller's: it ends the wait for Claude's model list
// (awaitClaudeModels), as the app's shutdown does.
func runTextGenWithFallback[T any](
	ctx context.Context,
	a *App,
	budget time.Duration,
	runOnce func(cfg textgen.Config, deadline time.Time) (T, error),
) (T, error) {
	// Cap total wall time across primary + all fallbacks to 2*budget so a
	// 3-provider fallback chain (e.g. commit messages with a 3m per-call
	// budget and 7m RPC timeout) cannot outlive the caller's RPC deadline.
	overallDeadline := time.Now().Add(budget * 2)

	// run runs runOnce on the config resolve returns, each run with its
	// attempt budget. A run whose admission found the account selection moved
	// since the config was resolved started no CLI, and one whose CLI an
	// account change ended has no result; both are resolved and run again: a
	// CLI runs only with the model chosen for the account it runs under. A
	// failed resolve still returns its config, which names the provider.
	// runOnce is called here and not handed on, so the budget walk
	// (TestEveryMethodReachingALongStepDeclaresItsBudget) follows each caller's
	// runOnce to its own run.
	run := func(attemptBudget time.Duration, resolve func(context.Context) (textgen.Config, error)) (textgen.Config, T, error) {
		for {
			remaining := time.Until(overallDeadline)
			if remaining <= 0 {
				var zero T
				return textgen.Config{}, zero, context.DeadlineExceeded
			}
			stepBudget := min(attemptBudget, remaining)
			stepDeadline := time.Now().Add(stepBudget)
			resolveCtx, cancelResolve := context.WithDeadline(ctx, stepDeadline)
			cfg, err := resolve(resolveCtx)
			cancelResolve()
			if err != nil {
				var zero T
				return cfg, zero, err
			}
			result, err := runOnce(cfg, stepDeadline)
			if errors.Is(err, procutil.ErrGroupSurvived) {
				// Logged here, once per run: a run an account change ended is
				// run again and a primary's failure gives way to a fallback
				// that succeeds, and either drops this error. One line: the
				// error joins the run's cause with the survivor.
				log.Printf(
					"textgen: a %s text-generation CLI (%s) could not be stopped and is still running: %s",
					cfg.Provider, filepath.Base(cfg.Binary), strings.ReplaceAll(textgen.RedactError(err), "\n", "; "),
				)
			}
			if !errors.Is(err, errTextGenerationSelectionMoved) {
				return cfg, result, err
			}
		}
	}
	primary, result, err := run(budget, func(stepCtx context.Context) (textgen.Config, error) {
		return a.resolveTextGenerationConfig(stepCtx)
	})
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
	fallbacks := textGenerationFallbackOrder(primary.Provider)
	for i, altName := range fallbacks {
		remaining := time.Until(overallDeadline)
		if remaining <= 0 {
			break
		}
		perAttempt := remaining / time.Duration(len(fallbacks)-i)
		if perAttempt > budget {
			perAttempt = budget
		}
		altCfg, altResult, altErr := run(perAttempt, func(stepCtx context.Context) (textgen.Config, error) {
			cfg, ok := a.resolveTextGenerationConfigFor(stepCtx, altName)
			if !ok {
				return cfg, errNoTextGenerationAlternate
			}
			return cfg, nil
		})
		if errors.Is(altErr, errNoTextGenerationAlternate) {
			continue
		}
		if altErr == nil {
			return altResult, nil
		}
		// No budget in the line: a timeout error from textgen already
		// names the CLI and the duration it was given.
		log.Printf(
			"textgen: fallback %s failed after primary %s error: %s",
			altCfg.Provider, primary.Provider, textgen.RedactError(altErr),
		)
	}
	return result, err
}

var (
	errTextGenerationSelectionMoved = errors.New("the provider account changed after the text-generation model was chosen")
	errNoTextGenerationAlternate    = errors.New("no alternate text-generation provider")
)

// textGenerationAdmission is a text-generation config's textgen.Admit for
// providerName. The selection generation is read before the config's model
// is, so any switch that could have changed the model moves it. The CLI then
// runs as a credential use of that generation
// (provideraccountapp.Manager.BeginCredentialUse): it starts only while the
// generation is still the provider's, and a switch, a removal or a finished
// sign-in ends it before changing the credential. Either way the run fails
// with errTextGenerationSelectionMoved, and runTextGenWithFallback chooses
// again. A switch never waits for a generation's work, only for its process
// to exit once stopped.
func (a *App) textGenerationAdmission(providerName string) textgen.Admission {
	resolvedUnder := a.providerAccounts.Generation(providerName)
	return func(ctx context.Context, stop func(error)) (func(), error) {
		release, err := a.providerAccounts.BeginCredentialUse(ctx, providerName, resolvedUnder, func() {
			stop(errTextGenerationSelectionMoved)
		})
		if errors.Is(err, provideraccountapp.ErrSelectionMoved) {
			return nil, errTextGenerationSelectionMoved
		}
		return release, err
	}
}

func textGenerationFallbackOrder(primary string) []string {
	switch primary {
	case string(provider.Claude):
		return []string{string(provider.Codex), string(provider.Agy)}
	case string(provider.Codex):
		return []string{string(provider.Claude), string(provider.Agy)}
	case string(provider.Agy):
		return []string{string(provider.Codex), string(provider.Claude)}
	default:
		return nil
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
	reaper := a.groupReaper("text generation")
	return func(ctx context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		return textgen.ExecCLI(ctx, reaper, spec)
	}
}
