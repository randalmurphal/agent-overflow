package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// writeProbeMockBinaryWithModels writes a Claude-CLI stand-in whose initialize
// response carries both an account and a `models` array — the same envelope
// the real CLI answers with, which is the whole point: one subprocess, two
// answers.
func writeProbeMockBinaryWithModels(t *testing.T, accountJSON, modelsJSON string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock-claude")

	inner := `{"account":` + accountJSON + `,"models":` + modelsJSON + `}`
	respLine := `{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"ao-probe-init","response":` + inner + `}}`
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		`printf '%s\n' '` + respLine + `'` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func appWithClaudeProbeBinary(t *testing.T, binary string) *App {
	t.Helper()
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	return app
}

func modelSlugs(models []provider.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.Slug)
	}
	return out
}

// TestGetModelsForProviderIsEnrichedByTheAccountProbe is the end-to-end wiring
// test for §2.7: the picker catalog grows a model the CLI reports and AO does
// not ship, and it does so off the zero-token account probe — no second
// subprocess anywhere in the path.
func TestGetModelsForProviderIsEnrichedByTheAccountProbe(t *testing.T) {
	binary := writeProbeMockBinaryWithModels(t,
		`{"subscriptionType":"Claude Max"}`,
		`[{"value":"opus","resolvedModel":"claude-opus-6[1m]","displayName":"Opus 6",`+
			`"supportsEffort":true,"supportedEffortLevels":["low","high","max"],"supportsFastMode":true}]`,
	)
	app := appWithClaudeProbeBinary(t, binary)

	before, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider before probe: %v", err)
	}
	if slices.Contains(modelSlugs(before), "claude-opus-6") {
		t.Fatal("the wire model must not appear before a probe has reported it")
	}
	if !slices.Equal(modelSlugs(before), modelSlugs(provider.ClaudeModels)) {
		t.Errorf("pre-probe catalog = %v, want the shipped list", modelSlugs(before))
	}

	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}

	after, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider after probe: %v", err)
	}
	if !slices.Contains(modelSlugs(after), "claude-opus-6") {
		t.Fatalf("wire-only model missing from the picker: %v", modelSlugs(after))
	}

	// Every shipped model survives — the wire list is a shortlist, and losing
	// a working model from the picker is the failure this feature must not
	// have.
	for _, shipped := range modelSlugs(provider.ClaudeModels) {
		if !slices.Contains(modelSlugs(after), shipped) {
			t.Errorf("%s dropped out of the catalog after enrichment", shipped)
		}
	}

	var opus6 provider.ModelInfo
	for _, model := range after {
		if model.Slug == "claude-opus-6" {
			opus6 = model
		}
	}
	if opus6.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", opus6.Provider)
	}
	if len(opus6.ContextWindows) != 2 {
		t.Errorf("ContextWindows = %v, want the claude-opus family's pair", opus6.ContextWindows)
	}
	if !slices.Contains(opus6.Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("fast-mode capability must survive the trip to the picker")
	}

	// claude-tui shares the binary and the login, so it shares the answer —
	// stamped as its own provider.
	tui, err := app.GetModelsForProvider("claude-tui")
	if err != nil {
		t.Fatalf("GetModelsForProvider(claude-tui): %v", err)
	}
	if !slices.Contains(modelSlugs(tui), "claude-opus-6") {
		t.Errorf("claude-tui catalog = %v, want the same enrichment", modelSlugs(tui))
	}
	for _, model := range tui {
		if model.Provider != "claude-tui" {
			t.Fatalf("claude-tui catalog carries a %q model", model.Provider)
		}
	}
}

// TestClaudeModelEnrichmentFollowsTheProbeIdentity: the enrichment is keyed by
// the probe's own cache key, so pointing the app at a different binary serves
// the shipped catalog again rather than the previous binary's model list.
func TestClaudeModelEnrichmentFollowsTheProbeIdentity(t *testing.T) {
	binary := writeProbeMockBinaryWithModels(t,
		`{"subscriptionType":"Claude Max"}`,
		`[{"value":"claude-newthing-1","displayName":"Newthing"}]`,
	)
	app := appWithClaudeProbeBinary(t, binary)
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}
	enriched, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider: %v", err)
	}
	if !slices.Contains(modelSlugs(enriched), "claude-newthing-1") {
		t.Fatalf("enrichment did not land: %v", modelSlugs(enriched))
	}

	other := writeProbeMockBinaryWithModels(t, `{}`, `[]`)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": other}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	models, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider after binary swap: %v", err)
	}
	if slices.Contains(modelSlugs(models), "claude-newthing-1") {
		t.Error("a different binary must not be served the previous binary's model list")
	}
	if !slices.Equal(modelSlugs(models), modelSlugs(provider.ClaudeModels)) {
		t.Errorf("catalog = %v, want the shipped list for an unprobed binary", modelSlugs(models))
	}
}

// TestClaudeProbeWithoutModelsLeavesTheCatalogAlone covers the older-CLI case:
// an initialize response with no `models` key is a real answer, and it must
// leave a usable picker rather than an empty one.
func TestClaudeProbeWithoutModelsLeavesTheCatalogAlone(t *testing.T) {
	app := appWithClaudeProbeBinary(t, writeProbeMockBinary(t, `{"subscriptionType":"pro"}`))
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}

	models, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider: %v", err)
	}
	if !slices.Equal(modelSlugs(models), modelSlugs(provider.ClaudeModels)) {
		t.Errorf("catalog = %v, want the shipped list untouched", modelSlugs(models))
	}
}

// TestClaudeModelCapabilityLookupsUseTheEnrichedCatalog proves the merge
// reaches the sanitizer, not just the picker: a thread on a wire-only model
// keeps the capabilities the wire granted it instead of being coerced by a
// catalog that has never heard of the model.
func TestClaudeModelCapabilityLookupsUseTheEnrichedCatalog(t *testing.T) {
	binary := writeProbeMockBinaryWithModels(t,
		`{"subscriptionType":"Claude Max"}`,
		`[{"value":"opus","resolvedModel":"claude-opus-6","displayName":"Opus 6",`+
			`"supportsEffort":true,"supportedEffortLevels":["low","medium"],"supportsFastMode":true}]`,
	)
	app := appWithClaudeProbeBinary(t, binary)
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}

	if !app.supportsFastModeForModel("claude", "claude-opus-6") {
		t.Error("fast mode must resolve through the enriched catalog")
	}
	if !app.reasoningEffortSupportedForModel("claude", "claude-opus-6", "medium") {
		t.Error("medium is on the wire for this model")
	}
	if app.reasoningEffortSupportedForModel("claude", "claude-opus-6", "max") {
		t.Error("max is not on the wire for this model")
	}
	// A catalog model the wire omits keeps every catalog capability: absence
	// from the shortlist is not a denial.
	if !app.supportsFastModeForModel("claude", "claude-opus-4-8") {
		t.Error("claude-opus-4-8 is absent from the wire but still fast-mode capable")
	}
	if !app.reasoningEffortSupportedForModel("claude", "claude-sonnet-4-6", "high") {
		t.Error("claude-sonnet-4-6 is absent from the wire but still supports high")
	}
}
