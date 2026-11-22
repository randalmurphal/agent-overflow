package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
)

// shippedNewestLuna is the newest gpt-luna model the shipped Codex catalog
// lists, what text generation runs on Codex when listCodexModels answers
// with that catalog.
const shippedNewestLuna = "gpt-5.6-luna"

// listCodexModels answers app's live Codex catalog with models, in place of
// the one App tests turn off. Call it before the app first reads a catalog.
func listCodexModels(app *App, models []provider.ModelInfo) {
	app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string, procutil.Reaper) ([]provider.ModelInfo, error) {
		return provider.CloneModels(models), nil
	}, time.Now)
}

// textGenConfig is the configured provider's text-generation config.
func textGenConfig(t *testing.T, app *App) textgen.Config {
	t.Helper()
	cfg, err := app.resolveTextGenerationConfig(context.Background())
	if err != nil {
		t.Fatalf("resolveTextGenerationConfig: %v", err)
	}
	return cfg
}

// reportedHaiku is the one Haiku claudeReportsModels has the Claude CLI
// list, what text generation runs on Claude after that report.
const reportedHaiku = "claude-haiku-4-5"

// claudeReportsModels has app's Claude CLI report its model list, with
// reportedHaiku its only Haiku, as its startup probe does. The catalog is
// process-wide: parallel tests that call it share it, and each stores
// the same list.
func claudeReportsModels(app *App) {
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{
		{Value: "default", ResolvedModel: "claude-opus-5-5"},
		{Value: "haiku", ResolvedModel: reportedHaiku},
	}, nil)
	capture.Store(app.claudeProbeModelKey())
}

func codexModelsNamed(slugs ...string) []provider.ModelInfo {
	models := make([]provider.ModelInfo, 0, len(slugs))
	for _, slug := range slugs {
		models = append(models, provider.ModelInfo{Slug: slug, Name: slug, Provider: string(provider.Codex)})
	}
	return models
}

// With settings leaving the model blank, text generation runs the newest
// model of its family the computer lists: Codex's newest Luna, and the
// newest Haiku the Claude CLI reports.
func TestTextGenerationRunsTheNewestModelOfItsFamily(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, codexModelsNamed("gpt-6.2-sol", "gpt-5.6-luna", "gpt-6-luna", "gpt-6.1-terra"))
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)

	if cfg := textGenConfig(t, app); cfg.Provider != string(provider.Codex) || cfg.Model != "gpt-6-luna" {
		t.Fatalf("codex text generation runs %s/%s, want codex/gpt-6-luna", cfg.Provider, cfg.Model)
	}
	for _, tc := range []struct {
		wire []claude.WireModel
		want string
	}{
		{[]claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}, {Value: "claude-haiku-4-5-20251001", ResolvedModel: "claude-haiku-4-5-20251001"}}, "claude-haiku-5-5"},
		{[]claude.WireModel{{Value: "claude-haiku-5-10", ResolvedModel: "claude-haiku-5-10"}, {Value: "haiku", ResolvedModel: "claude-haiku-5-5"}}, "claude-haiku-5-10"},
	} {
		capture := claudecatalog.ModelCapture{}
		capture.Capture(tc.wire, nil)
		capture.Store(app.claudeProbeModelKey())
		if cfg, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Claude)); !ok || cfg.Model != tc.want {
			t.Errorf("claude listing %v runs %q (ok %v), want %s", tc.wire, cfg.Model, ok, tc.want)
		}
	}
}

// claudeTitleApp is an app whose text generation runs on Claude, with
// Codex installed beside it and listing a Luna, and whose Claude CLI has
// not reported its models yet: the catalog is empty until the test stores
// a report. ran records each run as provider/model.
func claudeTitleApp(t *testing.T, wait time.Duration) (app *App, ran func() []string) {
	t.Helper()
	app = newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, codexModelsNamed("gpt-6-luna"))
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	app.claudeModelsWaitForTest = wait
	if _, err := app.settings.Update(map[string]any{"textGenerationProvider": "claude"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var runs []string
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		mu.Lock()
		runs = append(runs, spec.Binary+"/"+nextArgAfter(spec.Args, "--model"))
		mu.Unlock()
		if spec.Binary == "codex" {
			return textgen.CLIResult{}, errors.New("codex: no title here")
		}
		return textgen.CLIResult{Stdout: `{"structured_output":{"title":"From Claude"}}`}, nil
	}
	return app, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(runs)
	}
}

// A title requested before the Claude CLI has reported its models waits
// for the report and runs the newest Haiku it lists, not a model picked
// from the shipped list. Not parallel: the Claude catalog is process-wide.
func TestATitleRequestedBeforeClaudeReportsRunsTheNewestReportedHaiku(t *testing.T) {
	app, ran := claudeTitleApp(t, time.Minute)
	thread := testThread("title-before-report")
	thread.WorkspacePath = t.TempDir()
	type answer struct {
		title string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		title, err := app.generateThreadTitlePrompt(thread, threadtitle.BuildPrompt("Fix the report wait.", nil), nil)
		done <- answer{title, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the title ran before Claude reported its models: %+v, runs %v", got, ran())
	case <-time.After(200 * time.Millisecond):
	}
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}, {Value: "claude-haiku-6", ResolvedModel: "claude-haiku-6"}}, nil)
	capture.Store(app.claudeProbeModelKey())
	got := <-done
	if got.err != nil || got.title != "From Claude" {
		t.Fatalf("title = %q, %v", got.title, got.err)
	}
	if runs := ran(); !slices.Equal(runs, []string{"claude/claude-haiku-6"}) {
		t.Fatalf("runs %v, want one on claude-haiku-6", runs)
	}
}

// After an account switch, a title waits for the report of the account now
// in use: the list another account on the same binary reported serves the
// picker until then, but it is not what this account's CLI lists. Not
// parallel: the Claude catalog is process-wide.
func TestATitleAfterAnAccountSwitchWaitsForThatAccountsReport(t *testing.T) {
	app, ran := claudeTitleApp(t, time.Minute)
	other := app.claudeProbeModelKey()
	other.AccountID += "-before-the-switch"
	before := claudecatalog.ModelCapture{}
	before.Capture([]claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}}, nil)
	before.Store(other)
	if got := app.providerDiscoveryService().ClaudeCatalog(string(provider.Claude)).Provenance; got != provider.CatalogProbed {
		t.Fatalf("the picker's provenance is %q under the other account's list, want probed", got)
	}

	thread := testThread("title-after-switch")
	thread.WorkspacePath = t.TempDir()
	type answer struct {
		title string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		title, err := app.generateThreadTitlePrompt(thread, threadtitle.BuildPrompt("Fix the report wait.", nil), nil)
		done <- answer{title, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the title ran on the other account's list: %+v, runs %v", got, ran())
	case <-time.After(200 * time.Millisecond):
	}
	own := claudecatalog.ModelCapture{}
	own.Capture([]claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-6"}}, nil)
	own.Store(app.claudeProbeModelKey())
	got := <-done
	if got.err != nil || got.title != "From Claude" {
		t.Fatalf("title = %q, %v", got.title, got.err)
	}
	if runs := ran(); !slices.Equal(runs, []string{"claude/claude-haiku-6"}) {
		t.Fatalf("runs %v, want one on claude-haiku-6, the Haiku this account lists", runs)
	}
}

// Past the probe's bound with no report, the Claude attempt fails saying
// Claude has not reported its models, and Codex is tried. Not parallel:
// the Claude catalog is process-wide.
func TestATitleFailsOnClaudePastTheProbeBoundAndTriesCodex(t *testing.T) {
	const bound = 300 * time.Millisecond
	app, ran := claudeTitleApp(t, bound)
	start := time.Now()
	_, err := app.resolveTextGenerationConfig(context.Background())
	if waited := time.Since(start); err == nil || !strings.Contains(err.Error(), "Claude has not reported its models") || waited < bound {
		t.Fatalf("config after %s = %v, want Claude has not reported its models after the %s bound", waited, err, bound)
	}
	thread := testThread("title-past-bound")
	thread.WorkspacePath = t.TempDir()
	if _, err := app.generateThreadTitlePrompt(thread, threadtitle.BuildPrompt("Fix the report wait.", nil), nil); err == nil || !strings.Contains(err.Error(), "Claude has not reported its models") {
		t.Fatalf("title error = %v, want the Claude attempt's", err)
	}
	if runs := ran(); !slices.Equal(runs, []string{"codex/gpt-6-luna"}) {
		t.Fatalf("runs %v, want Codex tried once after Claude", runs)
	}
}

// The wait ends when the app shuts down, or when the caller's context
// ends, well before the bound, and Codex is not tried after either. Not
// parallel: the Claude catalog is process-wide.
func TestTheWaitForClaudesModelsEndsWithShutdownAndTheCaller(t *testing.T) {
	app, ran := claudeTitleApp(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan error, 1)
	go func() {
		_, err := runTextGenWithFallback(ctx, app, time.Minute, func(cfg textgen.Config, _ time.Time) (string, error) {
			return "", fmt.Errorf("ran %s", cfg.Provider)
		})
		called <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-called:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled caller's wait = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait outlived its caller's context")
	}

	thread := testThread("title-at-shutdown")
	thread.WorkspacePath = t.TempDir()
	shut := make(chan error, 1)
	go func() {
		_, err := app.generateThreadTitlePrompt(thread, threadtitle.BuildPrompt("Fix the report wait.", nil), nil)
		shut <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	app.appCancel()
	select {
	case err := <-shut:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the wait at shutdown = %v, want context.Canceled", err)
		}
		t.Logf("returned %s after shutdown", time.Since(start))
	case <-time.After(10 * time.Second):
		t.Fatal("the wait outlived the app")
	}
	if runs := ran(); len(runs) != 0 {
		t.Fatalf("runs %v, want none", runs)
	}
}

// The model text generation picks decides its effort from the same list it
// was picked from, not the shipped table: a live Luna the shipped catalog
// lacks that offers only medium and high runs medium (its default) for the
// low setting, one that offers no effort runs with none, and so does a
// Haiku the Claude CLI reports without effort levels.
func TestTextGenerationEffortFollowsTheModelItPicked(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	if _, err := app.settings.Update(map[string]any{"textGenerationProvider": "codex", "textGenerationReasoningEffort": "low"}); err != nil {
		t.Fatal(err)
	}
	luna := provider.ModelInfo{Slug: "gpt-7-luna", Name: "gpt-7-luna", Provider: string(provider.Codex), ReasoningEfforts: []provider.ReasoningEffortOption{
		{Slug: "medium", Label: "Medium", Default: true}, {Slug: "high", Label: "High"},
	}}
	plain := provider.ModelInfo{Slug: "gpt-8-luna", Name: "gpt-8-luna", Provider: string(provider.Codex)}
	for _, tc := range []struct {
		model provider.ModelInfo
		want  string
	}{{luna, "medium"}, {plain, ""}} {
		listCodexModels(app, []provider.ModelInfo{tc.model})
		if cfg := textGenConfig(t, app); cfg.Model != tc.model.Slug || cfg.Effort != tc.want {
			t.Errorf("codex text generation runs %s at effort %q, want %s at %q", cfg.Model, cfg.Effort, tc.model.Slug, tc.want)
		}
	}
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{{Value: "claude-haiku-6", ResolvedModel: "claude-haiku-6"}}, nil)
	capture.Store(app.claudeProbeModelKey())
	if cfg, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Claude)); !ok || cfg.Model != "claude-haiku-6" || cfg.Effort != "" {
		t.Errorf("claude text generation runs %s at effort %q (ok %v), want claude-haiku-6 with none", cfg.Model, cfg.Effort, ok)
	}
}

// A provider that lists no model of the family fails the attempt with a
// message naming what it lists, and the other provider runs instead.
func TestTextGenerationWithNoModelOfItsFamilyTurnsToTheOtherProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, codexModelsNamed("gpt-6.2-sol"))

	claudeReportsModels(app)
	if _, err := app.resolveTextGenerationConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "codex lists no gpt-luna model; it lists gpt-6.2-sol") {
		t.Fatalf("config error = %v, want codex lists no gpt-luna model", err)
	}
	var ran []string
	got, err := runTextGenWithFallback(context.Background(), app, time.Minute, func(cfg textgen.Config, _ time.Time) (string, error) {
		ran = append(ran, cfg.Provider+"/"+cfg.Model)
		return "title", nil
	})
	if err != nil || got != "title" || len(ran) != 1 || ran[0] != "claude/"+reportedHaiku {
		t.Fatalf("ran %v, got %q, %v; want only claude", ran, got, err)
	}
}

// fakeLookPath returns a function that resolves only the names in `available`.
// Everything else returns exec.ErrNotFound — matching the real exec.LookPath
// signature so the helpers can't tell production from test.
func fakeLookPath(available ...string) func(string) error {
	set := make(map[string]struct{}, len(available))
	for _, name := range available {
		set[name] = struct{}{}
	}
	return func(bin string) error {
		if _, ok := set[bin]; ok {
			return nil
		}
		return exec.ErrNotFound
	}
}

// TestResolveTextGenerationConfig_Layer1FallbackCodexMissing covers the
// canonical bug: default settings prefer Codex, but the user has only
// Claude installed. The resolver must substitute Claude with its own
// default model rather than handing the call site a
// Codex config that will fail on every exec.LookPath retry.
func TestResolveTextGenerationConfig_Layer1FallbackCodexMissing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude") // Codex not installed.

	claudeReportsModels(app)
	cfg := textGenConfig(t, app)
	if cfg.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want %q (fallback when codex missing)", cfg.Provider, provider.Claude)
	}
	if cfg.Model != reportedHaiku {
		t.Fatalf("model = %q, want %q (claude default after substitution)", cfg.Model, reportedHaiku)
	}
	if cfg.Binary != "claude" {
		t.Fatalf("binary = %q, want claude", cfg.Binary)
	}
}

func TestResolveTextGenerationConfig_Layer1FallbackClaudeMissing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	resetProviderBinarySettings(t, app)
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "claude",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex") // Claude not installed.
	listCodexModels(app, provider.CodexModels)

	cfg := textGenConfig(t, app)
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("provider = %q, want %q (fallback when claude missing)", cfg.Provider, provider.Codex)
	}
	if cfg.Model != shippedNewestLuna {
		t.Fatalf("model = %q, want %q", cfg.Model, shippedNewestLuna)
	}
}

func TestResolveTextGenerationConfig_BothAvailablePrefersConfigured(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, provider.CodexModels)

	cfg := textGenConfig(t, app)
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("default codex should win when both available: got %q", cfg.Provider)
	}
}

func TestResolveTextGenerationConfig_NeitherAvailableReturnsConfigured(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath() // nothing installed.

	cfg := textGenConfig(t, app)
	// Caller must surface the "codex CLI not found" error; we don't
	// invent a fake provider just to make the resolve step succeed.
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("neither installed: provider = %q, want %q (preserve user preference)", cfg.Provider, provider.Codex)
	}
}

// TestResolveTextGenerationConfig_DoesNotCarryModelAcrossProviders is the
// critical invariant: a Codex-named model in the user's settings must
// NOT be passed to Claude after Layer 1 substitution — Claude would
// reject the slug. The substituted provider always uses its own default.
func TestResolveTextGenerationConfig_DoesNotCarryModelAcrossProviders(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	resetProviderBinarySettings(t, app)
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "codex",
		"textGenerationModel":    "gpt-5.4-pro", // user-customized Codex model
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("claude") // only Claude installed.

	claudeReportsModels(app)
	cfg := textGenConfig(t, app)
	if cfg.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want claude", cfg.Provider)
	}
	if cfg.Model == "gpt-5.4-pro" {
		t.Fatalf("Codex model %q must NOT carry across to Claude", cfg.Model)
	}
	if cfg.Model != reportedHaiku {
		t.Fatalf("model = %q, want %q (claude default)", cfg.Model, reportedHaiku)
	}
}

// TestResolveTextGenerationConfig_KeepsUserModelWhenProviderUnchanged
// is the negative of the above: when the configured provider IS available,
// the user's explicit model setting is honored. Substitution is what
// triggers the reset; happy-path keeps user choice.
func TestResolveTextGenerationConfig_KeepsUserModelWhenProviderUnchanged(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	resetProviderBinarySettings(t, app)
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "codex",
		"textGenerationModel":    "gpt-5.4-pro",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex") // configured provider available.

	cfg := textGenConfig(t, app)
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("provider = %q, want codex", cfg.Provider)
	}
	if cfg.Model != "gpt-5.4-pro" {
		t.Fatalf("user model dropped: got %q, want gpt-5.4-pro", cfg.Model)
	}
}

func TestResolveTextGenerationConfig_CoercesEffortForKnownCodexModel(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	resetProviderBinarySettings(t, app)
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationModel":           "gpt-5.5",
		"textGenerationReasoningEffort": "ultra",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex")

	cfg := textGenConfig(t, app)
	if cfg.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", cfg.Model)
	}
	if cfg.Effort != string(provider.EffortMedium) {
		t.Fatalf("effort = %q, want medium (gpt-5.5 default)", cfg.Effort)
	}
}

func TestResolveTextGenerationConfig_PreservesUltraForSol(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	resetProviderBinarySettings(t, app)
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationModel":           "gpt-5.6-sol",
		"textGenerationReasoningEffort": "ultra",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex")

	cfg := textGenConfig(t, app)
	if cfg.Model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", cfg.Model)
	}
	if cfg.Effort != string(provider.EffortUltra) {
		t.Fatalf("effort = %q, want ultra", cfg.Effort)
	}
}

func TestResolveTextGenerationConfigFor_ReturnsFalseWhenBinaryMissing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude")

	claudeReportsModels(app)
	if _, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Codex)); ok {
		t.Fatalf("resolveTextGenerationConfigFor(codex) ok = true, want false (binary missing)")
	}
	cfg, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Claude))
	if !ok {
		t.Fatalf("resolveTextGenerationConfigFor(claude) ok = false; expected true (binary present)")
	}
	if cfg.Model != reportedHaiku {
		t.Fatalf("model = %q, want %q (always default for alternate)", cfg.Model, reportedHaiku)
	}
}

// TestResolveTextGenerationConfigFor_SymmetricClaudeMissing mirrors the above
// for the other axis: only Codex installed → Codex resolves, Claude returns
// ok=false. Catches regressions where one branch loses its lookPath check.
func TestResolveTextGenerationConfigFor_SymmetricClaudeMissing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("codex")
	listCodexModels(app, provider.CodexModels)

	if _, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Claude)); ok {
		t.Fatalf("resolveTextGenerationConfigFor(claude) ok = true, want false (binary missing)")
	}
	cfg, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Codex))
	if !ok {
		t.Fatalf("resolveTextGenerationConfigFor(codex) ok = false; expected true (binary present)")
	}
	if cfg.Model != shippedNewestLuna {
		t.Fatalf("model = %q, want %q (always default for alternate)", cfg.Model, shippedNewestLuna)
	}
}

func TestResolveTextGenerationConfigFor_UnknownProviderReturnsFalse(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")
	if _, ok := app.resolveTextGenerationConfigFor(context.Background(), "phantom"); ok {
		t.Fatalf("unknown provider should return ok=false")
	}
}

// TestRunTextGenWithFallback_PerAttemptBudgets is the incident fix
// (2026-08-16): a primary that fails by TIMING OUT must still reach the
// alternate, and the alternate must get its own full budget rather than
// the primary's exhausted remainder.
func TestRunTextGenWithFallback_PerAttemptBudgets(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, provider.CodexModels)

	const budget = 90 * time.Second
	var deadlines []time.Time
	var providers []string

	start := time.Now()
	claudeReportsModels(app)
	got, err := runTextGenWithFallback(context.Background(), app, budget,
		func(cfg textgen.Config, deadline time.Time) (string, error) {
			providers = append(providers, cfg.Provider)
			deadlines = append(deadlines, deadline)
			if len(providers) == 1 {
				return "", fmt.Errorf("primary: %w", context.DeadlineExceeded)
			}
			return "alternate result", nil
		},
	)
	if err != nil {
		t.Fatalf("runTextGenWithFallback() error = %v", err)
	}
	if got != "alternate result" {
		t.Fatalf("result = %q, want the alternate's", got)
	}
	if len(providers) != 2 {
		t.Fatalf("attempts = %v, want primary + alternate (DeadlineExceeded must not short-circuit)", providers)
	}
	if providers[0] == providers[1] {
		t.Fatalf("both attempts used %q", providers[0])
	}
	// Each attempt's deadline is ~now+budget. The second must be a FRESH
	// budget, not the first's remainder — and the tolerance is tight
	// enough to notice a shared one: two in-process attempts take
	// microseconds, so seconds of slack is all the wall clock needs.
	for i, deadline := range deadlines {
		if delta := deadline.Sub(start); delta < budget || delta > budget+5*time.Second {
			t.Fatalf("attempt %d deadline is %s past start, want ~%s", i+1, delta, budget)
		}
	}
	if !deadlines[1].After(deadlines[0]) {
		t.Fatal("alternate deadline is not later than the primary's — the budget was shared, not fresh")
	}
}

// TestRunTextGenWithFallback_AlternateFailureIsLoggedAndPrimarySurfaces
// pins the other half of the incident: the fallback's failure used to be
// discarded silently, leaving one opaque log line about the primary.
func TestRunTextGenWithFallback_AlternateFailureIsLoggedAndPrimarySurfaces(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, provider.CodexModels)

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	attempts := 0
	claudeReportsModels(app)
	_, err := runTextGenWithFallback(context.Background(), app, 30*time.Second,
		func(cfg textgen.Config, _ time.Time) (string, error) {
			attempts++
			if attempts == 1 {
				return "", errors.New("primary exploded")
			}
			// What a real CLI timeout looks like after textgen translates
			// it: the message names the CLI and the budget it had.
			return "", textgen.TranslateCLINotFound(cfg.Provider, 30*time.Second, context.DeadlineExceeded)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "primary exploded") {
		t.Fatalf("err = %v, want the primary's error surfaced", err)
	}
	line := logged.String()
	if !strings.Contains(line, "textgen: fallback") {
		t.Fatalf("fallback failure not logged: %q", line)
	}
	if !strings.Contains(line, "CLI timed out after 30s") {
		t.Fatalf("log line should report the alternate's timeout: %q", line)
	}
}

// TestRunTextGenWithFallback_NoAlternateSpawnDuringShutdown: a CLI
// killed by teardown reports its own failure rather than
// context.Canceled, and starting a second provider subprocess into a
// shutdown is how orphans are made.
func TestRunTextGenWithFallback_NoAlternateSpawnDuringShutdown(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")
	listCodexModels(app, provider.CodexModels)

	attempts := 0
	_, err := runTextGenWithFallback(context.Background(), app, time.Minute,
		func(textgen.Config, time.Time) (string, error) {
			attempts++
			app.appCancel()
			return "", errors.New("codex CLI failed: signal: killed")
		},
	)
	if err == nil {
		t.Fatal("expected the primary error")
	}
	if attempts != 1 {
		t.Fatalf("executor called %d times, want 1 (no fallback spawn during shutdown)", attempts)
	}
}

func TestOtherProvider(t *testing.T) {
	t.Parallel()
	if got := otherProvider(string(provider.Claude)); got != string(provider.Codex) {
		t.Fatalf("otherProvider(claude) = %q, want codex", got)
	}
	if got := otherProvider(string(provider.Codex)); got != string(provider.Claude) {
		t.Fatalf("otherProvider(codex) = %q, want claude", got)
	}
	if got := otherProvider("bogus"); got != "" {
		t.Fatalf("otherProvider(bogus) = %q, want empty", got)
	}
}

func TestAvailableTextGenerationProviders(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)

	// Order matters: chatmodel.FallbackProvider prefers the first known
	// entry it sees, and the caller (seedChatModelProfile) feeds this
	// list straight through. Claude must come before Codex so that the
	// "both installed" case keeps the historical Claude-first default.
	app.lookPathFn = fakeLookPath("claude", "codex")
	got := app.availableTextGenerationProviders()
	if len(got) != 2 || got[0] != string(provider.Claude) || got[1] != string(provider.Codex) {
		t.Fatalf("both installed: got %v, want [claude codex] in order", got)
	}

	app.lookPathFn = fakeLookPath("claude")
	got = app.availableTextGenerationProviders()
	if len(got) != 1 || got[0] != string(provider.Claude) {
		t.Fatalf("only claude installed: got %v, want [claude]", got)
	}

	app.lookPathFn = fakeLookPath("codex")
	got = app.availableTextGenerationProviders()
	if len(got) != 1 || got[0] != string(provider.Codex) {
		t.Fatalf("only codex installed: got %v, want [codex]", got)
	}

	app.lookPathFn = fakeLookPath()
	got = app.availableTextGenerationProviders()
	if len(got) != 0 {
		t.Fatalf("neither installed: got %v, want []", got)
	}
}
