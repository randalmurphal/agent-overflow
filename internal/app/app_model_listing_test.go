package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// seedClaudeWire stores one probe answer under the app's Claude identity, the
// way the account probe's capture does. Tests using it are not parallel: the
// probe catalog is process-wide.
func seedClaudeWire(t *testing.T, app *App, wire []claude.WireModel) {
	t.Helper()
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	capture := claudecatalog.ModelCapture{}
	capture.Capture(wire, nil)
	capture.Store(app.claudeProbeModelKey())
}

// A model the CLI's latest answer omits leaves the picker unless a thread ran
// it recently; either way it still resolves capabilities for threads on it.
func TestGetModelsForProviderKeepsRecentlyUsedModelsListed(t *testing.T) {
	app := newTestAppWithStore(t)
	seedClaudeWire(t, app, []claude.WireModel{
		{Value: "default", ResolvedModel: "claude-opus-5"},
		{Value: "claude-opus-5", ResolvedModel: "claude-opus-5"},
	})
	now := time.Now()
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		{CreatedAt: now.UnixMilli(), ThreadID: "t-recent", TurnID: "1", Provider: "claude", Model: "claude-opus-4-7[1m]"},
		{CreatedAt: now.Add(-recentModelUseWindow - time.Hour).UnixMilli(), ThreadID: "t-old", TurnID: "1", Provider: "claude", Model: "claude-opus-4-6"},
	}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	catalog, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatalf("GetModelsForProvider: %v", err)
	}
	unlisted := map[string]bool{}
	for _, m := range catalog.Models {
		unlisted[m.Slug] = m.Unlisted
	}
	for slug, want := range map[string]bool{
		"claude-opus-5":   false, // listed by the CLI
		"claude-opus-4-7": false, // omitted, but ran within the window
		"claude-opus-4-6": true,  // omitted, last ran before the window
		"claude-opus-4-8": true,  // omitted, never ran
	} {
		got, ok := unlisted[slug]
		if !ok {
			t.Errorf("%s missing from the served catalog; it must stay for capability lookup", slug)
			continue
		}
		if got != want {
			t.Errorf("%s Unlisted = %v, want %v", slug, got, want)
		}
	}
	if _, found := app.modelInfoWithoutLiveCodex(string(provider.Claude), "claude-opus-4-6"); !found {
		t.Error("an unlisted model no longer resolves capabilities")
	}
}

// A new thread with no remembered choice starts on the model the CLI names as
// its default, and thread_options reports only listed models with that
// default, while a spawn naming an unlisted model is still accepted.
func TestProviderDefaultAndListedOptionsFollowTheCLI(t *testing.T) {
	app := newTestAppWithStore(t)
	seedClaudeWire(t, app, []claude.WireModel{
		{Value: "default", ResolvedModel: "claude-opus-4-8[1m]"},
		{Value: "claude-sonnet-5", ResolvedModel: "claude-sonnet-5"},
		{Value: "opus", ResolvedModel: "claude-opus-4-8"},
	})

	if got := app.defaultModelForProvider(string(provider.Claude)); got != "claude-opus-4-8" {
		t.Fatalf("defaultModelForProvider = %q, want the CLI default claude-opus-4-8", got)
	}
	if got := app.fallbackChatModelProfile(string(provider.Claude), "").Model; got != "claude-opus-4-8" {
		t.Fatalf("fallback profile model = %q, want claude-opus-4-8", got)
	}

	option, err := threadToolsApp{app: app}.providerOption(string(provider.Claude))
	if err != nil {
		t.Fatalf("providerOption: %v", err)
	}
	var got []string
	for _, m := range option.Models {
		got = append(got, m.Slug)
	}
	// CLI order: the default row resolves to opus-4-8 and comes first.
	if len(got) != 2 || got[0] != "claude-opus-4-8" || got[1] != "claude-sonnet-5" {
		t.Fatalf("thread_options models = %v, want [claude-opus-4-8 claude-sonnet-5]", got)
	}
	if option.DefaultModel != "claude-opus-4-8" {
		t.Fatalf("thread_options default = %q, want claude-opus-4-8", option.DefaultModel)
	}
	if err := app.validateModelChoice(string(provider.Claude), "claude-opus-4-5"); err != nil {
		t.Fatalf("an unlisted model was refused as input: %v", err)
	}
	// An agent spawn or fork naming an unlisted model a thread runs is
	// accepted, though thread_options does not offer it.
	opts := CreateThreadOptions{Provider: string(provider.Claude)}
	if err := (threadToolsApp{app: app}).applySpawnModel(&opts, threadtools.SpawnCall{Model: "claude-opus-4-7"}); err != nil {
		t.Fatalf("spawn on an unlisted model refused: %v", err)
	}
	if opts.Model != "claude-opus-4-7" {
		t.Fatalf("spawn model = %q, want claude-opus-4-7", opts.Model)
	}
}

// With no CLI answer the shipped catalog stands and nothing is hidden.
func TestGetModelsForProviderBeforeAnyAnswerListsTheShippedCatalog(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	app := newTestAppWithStore(t)
	catalog, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatalf("GetModelsForProvider: %v", err)
	}
	if catalog.Provenance != provider.CatalogShipped {
		t.Fatalf("provenance = %q, want shipped", catalog.Provenance)
	}
	for _, m := range catalog.Models {
		if m.Unlisted {
			t.Fatalf("%s unlisted with no CLI answer", m.Slug)
		}
	}
	if got := app.defaultModelForProvider(string(provider.Claude)); got != provider.ClaudeModels[0].Slug {
		t.Fatalf("default with no answer = %q, want the shipped head %q", got, provider.ClaudeModels[0].Slug)
	}
}

// A startup Claude probe that fails is retried until one succeeds, and the
// model list that success reports reaches the served catalog. Not parallel:
// it shortens a package-level backoff and resets process-wide caches.
func TestStartupClaudeProbeRetriesUntilTheModelListArrives(t *testing.T) {
	resetClaudeProbeCacheForTest(t)
	previous := claudeStartupProbeBackoff
	claudeStartupProbeBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { claudeStartupProbeBackoff = previous })

	app := newTestAppWithStore(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	attempts := filepath.Join(dir, "attempts")
	// Fails twice (no answer), then answers initialize with a model list.
	response := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init",` +
		`"response":{"account":{"subscriptionType":"pro","tokenSource":"oauth"},` +
		`"models":[{"value":"default","resolvedModel":"claude-opus-4-8"},{"value":"claude-opus-4-8","resolvedModel":"claude-opus-4-8"}]}}}`
	writeProviderBinaryFile(t, binary,
		"echo x >> '"+attempts+"'\n"+
			"[ \"$(wc -l < '"+attempts+"')\" -le 2 ] && exit 1\n"+
			"read -r _ || true\nprintf '%s\\n' '"+response+"'\nexit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if !app.retryClaudeStartupProbe(ctx) {
		t.Fatal("retry loop gave up before the probe succeeded")
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if n := strings.Count(string(data), "x"); n != 3 {
		t.Fatalf("probe ran %d times, want 3 (two failures, one success)", n)
	}
	if got := app.defaultModelForProvider(string(provider.Claude)); got != "claude-opus-4-8" {
		t.Fatalf("default after the retried probe = %q, want the CLI default claude-opus-4-8", got)
	}
}

// With no binary installed the loop spawns nothing and still exits on cancel.
func TestStartupClaudeProbeRetryWaitsForAnInstalledBinary(t *testing.T) {
	resetClaudeProbeCacheForTest(t)
	previous := claudeStartupProbeBackoff
	claudeStartupProbeBackoff = []time.Duration{5 * time.Millisecond}
	t.Cleanup(func() { claudeStartupProbeBackoff = previous })

	app := newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": filepath.Join(t.TempDir(), "missing")}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if app.retryClaudeStartupProbe(ctx) {
		t.Fatal("a missing binary reported a successful probe")
	}
}
