package provider

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestModelsForProvider_Claude(t *testing.T) {
	models := ModelsForProvider("claude")
	if models == nil {
		t.Fatal("expected non-nil slice for claude")
	}
	if len(models) != len(ClaudeModels) {
		t.Fatalf("got %d models, want %d", len(models), len(ClaudeModels))
	}
	for i, m := range models {
		if m.Slug != ClaudeModels[i].Slug {
			t.Errorf("model[%d].Slug = %q, want %q", i, m.Slug, ClaudeModels[i].Slug)
		}
		if m.Provider != "claude" {
			t.Errorf("model[%d].Provider = %q, want %q", i, m.Provider, "claude")
		}
		if !slices.Equal(m.Capabilities, ClaudeModels[i].Capabilities) {
			t.Errorf("model[%d].Capabilities = %v, want %v", i, m.Capabilities, ClaudeModels[i].Capabilities)
		}
	}
}

func TestModelsForProvider_Codex(t *testing.T) {
	models := ModelsForProvider("codex")
	if models == nil {
		t.Fatal("expected non-nil slice for codex")
	}
	if len(models) != len(CodexModels) {
		t.Fatalf("got %d models, want %d", len(models), len(CodexModels))
	}
	for i, m := range models {
		if m.Slug != CodexModels[i].Slug {
			t.Errorf("model[%d].Slug = %q, want %q", i, m.Slug, CodexModels[i].Slug)
		}
		if m.Provider != "codex" {
			t.Errorf("model[%d].Provider = %q, want %q", i, m.Provider, "codex")
		}
		if !slices.Equal(m.Capabilities, CodexModels[i].Capabilities) {
			t.Errorf("model[%d].Capabilities = %v, want %v", i, m.Capabilities, CodexModels[i].Capabilities)
		}
	}
}

func TestModelsForProvider_ClaudeTUI(t *testing.T) {
	models := ModelsForProvider(string(ClaudeTUI))
	if models == nil {
		t.Fatal("expected non-nil slice for claude-tui")
	}
	// claude-tui drives the same binary, so its catalog mirrors claude's
	// (same slugs / capabilities / windows / efforts) but is stamped claude-tui.
	if len(models) != len(ClaudeModels) {
		t.Fatalf("got %d models, want %d (parity with claude)", len(models), len(ClaudeModels))
	}
	for i, m := range models {
		if m.Slug != ClaudeModels[i].Slug {
			t.Errorf("model[%d].Slug = %q, want %q", i, m.Slug, ClaudeModels[i].Slug)
		}
		if m.Provider != string(ClaudeTUI) {
			t.Errorf("model[%d].Provider = %q, want %q", i, m.Provider, string(ClaudeTUI))
		}
		if !slices.Equal(m.Capabilities, ClaudeModels[i].Capabilities) {
			t.Errorf("model[%d].Capabilities = %v, want %v", i, m.Capabilities, ClaudeModels[i].Capabilities)
		}
		if !slices.Equal(m.ReasoningEfforts, ClaudeModels[i].ReasoningEfforts) {
			t.Errorf("model[%d].ReasoningEfforts mismatch with claude", i)
		}
		if !slices.Equal(m.ContextWindows, ClaudeModels[i].ContextWindows) {
			t.Errorf("model[%d].ContextWindows mismatch with claude", i)
		}
	}
	// Stamping claude-tui must not mutate the shared ClaudeModels source.
	for i, m := range ClaudeModels {
		if m.Provider != "claude" {
			t.Errorf("ClaudeModels[%d].Provider = %q, want claude (withProvider must clone, not mutate)", i, m.Provider)
		}
	}
}

func TestClaudeTUIResolvesLikeClaude(t *testing.T) {
	// Alias normalization is shared with claude.
	if got := NormalizeModelSlug(string(ClaudeTUI), "opus"); got != "claude-opus-5" {
		t.Errorf("NormalizeModelSlug(claude-tui, opus) = %q, want claude-opus-5", got)
	}
	if _, found := FindModel(string(ClaudeTUI), "claude-opus-4-8"); !found {
		t.Error("FindModel(claude-tui, claude-opus-4-8) not found")
	}
	// Shares claude's effort set; rejects a codex-only effort.
	if !ReasoningEffortSupportedForModel(string(ClaudeTUI), "claude-opus-4-8", "max") {
		t.Error("claude-tui should support effort 'max'")
	}
	if ReasoningEffortSupportedForModel(string(ClaudeTUI), "claude-opus-4-8", "minimal") {
		t.Error("claude-tui must not support codex-only effort 'minimal'")
	}
	if got := DefaultReasoningEffortForModel(string(ClaudeTUI), "claude-opus-4-8", DefaultReasoningEffort); got != EffortXHigh {
		t.Errorf("default effort for claude-tui opus-4-8 = %q, want xhigh", got)
	}
	// Static catalog, no background cleaner — identical posture to headless claude.
	if caps := CapabilitiesForProvider(string(ClaudeTUI)); caps.ModelCatalog == CodexLiveModelCatalog {
		t.Errorf("claude-tui must use the static catalog, got %q", caps.ModelCatalog)
	}
}

func TestCodexModelsDefaultToGPT56Sol(t *testing.T) {
	models := ModelsForProvider("codex")
	if len(models) == 0 {
		t.Fatal("expected codex models")
	}
	if models[0].Slug != "gpt-5.6-sol" {
		t.Fatalf("first codex model = %q, want gpt-5.6-sol", models[0].Slug)
	}
	if !slices.ContainsFunc(models, func(model ModelInfo) bool {
		return model.Slug == "gpt-5.5"
	}) {
		t.Fatalf("codex models missing gpt-5.5: %#v", models)
	}
}

func TestClaudeFastModeAndContextCapabilities(t *testing.T) {
	cases := []struct {
		model    string
		fastMode bool
		windows  int
	}{
		{"claude-fable-5", false, 2},
		{"claude-opus-5", true, 2},
		{"claude-opus-4-8", true, 2},
		{"claude-opus-4-7", true, 2},
		{"claude-opus-4-6", true, 2},
		{"claude-opus-4-5", false, 2},
		{"claude-sonnet-5", false, 2},
		{"claude-sonnet-4-6", false, 2},
		{"claude-haiku-4-5", false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			model, found := FindModel("claude", tc.model)
			if !found {
				t.Fatalf("FindModel(%q) not found", tc.model)
			}
			if got := slices.Contains(model.Capabilities, ModelCapabilityFastMode); got != tc.fastMode {
				t.Fatalf("fast capability = %v, want %v", got, tc.fastMode)
			}
			if len(model.ContextWindows) != tc.windows {
				t.Fatalf("ContextWindows len = %d, want %d", len(model.ContextWindows), tc.windows)
			}
			// Slice order is picker order and is deliberately smallest-first;
			// which tier is the *default* is the Default flag, asserted in
			// TestClaudeDefaultContextWindowPerModel.
			if model.ContextWindows[0].Tokens != ClaudeStandardContextWindow {
				t.Fatalf("first ContextWindow option = %d, want %d", model.ContextWindows[0].Tokens, ClaudeStandardContextWindow)
			}
		})
	}
}

func TestClaudeSonnetEffortTiersMatchAPICapabilities(t *testing.T) {
	// Pins the effort tiers documented on the claude-sonnet-5 and
	// claude-sonnet-4-6 catalog entries in models.go (4.6 deliberately
	// lacks xhigh — see the comment there).
	cases := []struct {
		model   string
		efforts []string
		def     string
	}{
		{"claude-sonnet-5", []string{"low", "medium", "high", "xhigh", "max"}, "high"},
		{"claude-sonnet-4-6", []string{"low", "medium", "high", "max"}, "high"},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			model, found := FindModel("claude", tc.model)
			if !found {
				t.Fatalf("FindModel(%q) not found", tc.model)
			}
			slugs := make([]string, len(model.ReasoningEfforts))
			for i, option := range model.ReasoningEfforts {
				slugs[i] = option.Slug
			}
			if !slices.Equal(slugs, tc.efforts) {
				t.Fatalf("effort slugs = %v, want %v", slugs, tc.efforts)
			}
			if got := string(DefaultReasoningEffortForModel("claude", tc.model, DefaultReasoningEffort)); got != tc.def {
				t.Fatalf("default effort = %q, want %q", got, tc.def)
			}
		})
	}

	// A stale persisted xhigh on 4.6 must coerce to the default, while
	// the tiers each model genuinely supports pass through untouched.
	coercions := []struct {
		model  string
		effort ReasoningEffort
		want   ReasoningEffort
	}{
		{"claude-sonnet-4-6", EffortXHigh, EffortHigh},
		{"claude-sonnet-4-6", EffortMax, EffortMax},
		{"claude-sonnet-5", EffortXHigh, EffortXHigh},
		{"claude-sonnet-5", EffortMax, EffortMax},
	}
	for _, tc := range coercions {
		if got := CoerceReasoningEffortForModel("claude", tc.model, tc.effort); got != tc.want {
			t.Errorf("CoerceReasoningEffortForModel(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestSonnetAliasResolvesToSonnet5(t *testing.T) {
	// The bare "sonnet" alias must resolve to the Sonnet 5 catalog entry
	// (with its capabilities), not just normalize at the string level.
	model, found := FindModel("claude", "sonnet")
	if !found {
		t.Fatal(`FindModel("claude", "sonnet") not found`)
	}
	if model.Slug != "claude-sonnet-5" {
		t.Fatalf(`FindModel("claude", "sonnet").Slug = %q, want claude-sonnet-5`, model.Slug)
	}
	if !ReasoningEffortSupportedForModel("claude", "sonnet", "xhigh") {
		t.Error(`effort "xhigh" should be supported via the "sonnet" alias (Sonnet 5)`)
	}
}

func TestOpusAliasResolvesToOpus5(t *testing.T) {
	// The bare "opus" alias must resolve to the Opus 5 catalog entry
	// (with its capabilities), not just normalize at the string level.
	model, found := FindModel("claude", "opus")
	if !found {
		t.Fatal(`FindModel("claude", "opus") not found`)
	}
	if model.Slug != "claude-opus-5" {
		t.Fatalf(`FindModel("claude", "opus").Slug = %q, want claude-opus-5`, model.Slug)
	}
	if !slices.Contains(model.Capabilities, ModelCapabilityFastMode) {
		t.Error(`fast mode should be supported via the "opus" alias (Opus 5)`)
	}
	if got := DefaultReasoningEffortForModel("claude", "claude-opus-5", DefaultReasoningEffort); got != EffortXHigh {
		t.Errorf("default effort for claude-opus-5 = %q, want xhigh", got)
	}
}

func TestCodexModelCapabilitiesAndContextWindows(t *testing.T) {
	cases := []struct {
		model    string
		name     string
		fastMode bool
		windows  []int
		def      string
		efforts  []string
	}{
		{"gpt-5.6-sol", "GPT 5.6 Sol", true, []int{Codex56ContextWindow}, "low", []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{"gpt-5.6-terra", "GPT 5.6 Terra", true, []int{Codex56ContextWindow}, "medium", []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{"gpt-5.6-luna", "GPT 5.6 Luna", true, []int{Codex56ContextWindow}, "medium", []string{"low", "medium", "high", "xhigh", "max"}},
		{"gpt-5.4", "GPT 5.4", true, []int{CodexStandardContextWindow, CodexExtendedContextWindow}, "xhigh", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.5", "GPT 5.5", true, []int{CodexStandardContextWindow}, "medium", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.2", "GPT 5.2", false, []int{CodexStandardContextWindow}, "medium", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.3-codex", "GPT 5.3 Codex", false, []int{CodexStandardContextWindow}, "medium", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.4-mini", "GPT 5.4 Mini", false, []int{CodexStandardContextWindow}, "medium", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.3-codex-spark", "GPT 5.3 Codex Spark", false, []int{CodexSparkContextWindow}, "high", []string{"low", "medium", "high", "xhigh"}},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			model, found := FindModel("codex", tc.model)
			if !found {
				t.Fatalf("FindModel(%q) not found", tc.model)
			}
			if model.Name != tc.name {
				t.Fatalf("Name = %q, want %q", model.Name, tc.name)
			}
			if got := slices.Contains(model.Capabilities, ModelCapabilityFastMode); got != tc.fastMode {
				t.Fatalf("fast capability = %v, want %v", got, tc.fastMode)
			}
			if len(model.ContextWindows) != len(tc.windows) {
				t.Fatalf("ContextWindows len = %d, want %d: %#v", len(model.ContextWindows), len(tc.windows), model.ContextWindows)
			}
			for i, tokens := range tc.windows {
				if model.ContextWindows[i].Tokens != tokens {
					t.Fatalf("ContextWindows[%d].Tokens = %d, want %d", i, model.ContextWindows[i].Tokens, tokens)
				}
			}
			if got := string(DefaultReasoningEffortForModel("codex", tc.model, DefaultReasoningEffort)); got != tc.def {
				t.Fatalf("default reasoning = %q, want %q", got, tc.def)
			}
			slugs := make([]string, len(model.ReasoningEfforts))
			for i, option := range model.ReasoningEfforts {
				slugs[i] = option.Slug
			}
			if !slices.Equal(slugs, tc.efforts) {
				t.Fatalf("effort slugs = %v, want %v", slugs, tc.efforts)
			}
		})
	}
}

func TestCodexFallbackReasoningLabelsAreTierNames(t *testing.T) {
	model, found := FindModel("codex", "gpt-5.5")
	if !found {
		t.Fatal("gpt-5.5 not found")
	}
	want := []ReasoningEffortOption{
		{Slug: "low", Label: "Low"},
		{Slug: "medium", Label: "Medium", Default: true},
		{Slug: "high", Label: "High"},
		{Slug: "xhigh", Label: "xHigh"},
	}
	if !slices.Equal(model.ReasoningEfforts, want) {
		t.Fatalf("ReasoningEfforts = %#v, want %#v", model.ReasoningEfforts, want)
	}
}

func TestModelInfoReasoningEffortHelpersIgnoreUnknownLiveSlugs(t *testing.T) {
	model := ModelInfo{
		Provider: string(Codex),
		ReasoningEfforts: []ReasoningEffortOption{
			{Slug: "future-effort", Default: true},
			{Slug: "ultra"},
		},
	}
	if ModelInfoSupportsReasoningEffort(model, "future-effort") {
		t.Fatal("unknown live effort should not pass the app's persisted enum")
	}
	if got := CoerceReasoningEffortForModelInfo(model, EffortHigh); got != EffortUltra {
		t.Fatalf("CoerceReasoningEffortForModelInfo = %q, want first known effort %q", got, EffortUltra)
	}
}

func TestNormalizeModelSlugClaudeAliases(t *testing.T) {
	tests := map[string]string{
		"fable":                      "claude-fable-5",
		"fable-5":                    "claude-fable-5",
		"claude-fable-5":             "claude-fable-5",
		"opus":                       "claude-opus-5",
		"opus-5":                     "claude-opus-5",
		"claude-opus-5":              "claude-opus-5",
		"opus-4.8":                   "claude-opus-4-8",
		"claude-opus-4.8":            "claude-opus-4-8",
		"opus-4.7":                   "claude-opus-4-7",
		"claude-opus-4.7":            "claude-opus-4-7",
		"claude-opus-4.6":            "claude-opus-4-6",
		"sonnet":                     "claude-sonnet-5",
		"sonnet-5":                   "claude-sonnet-5",
		"claude-sonnet-5":            "claude-sonnet-5",
		"sonnet-4.6":                 "claude-sonnet-4-6",
		"claude-sonnet-4.6":          "claude-sonnet-4-6",
		"haiku":                      "claude-haiku-4-5",
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
		"claude-opus-4-6-20251117":   "claude-opus-4-6-20251117",
		"claude-sonnet-4-6-20251117": "claude-sonnet-4-6-20251117",
	}

	for input, want := range tests {
		if got := NormalizeModelSlug("claude", input); got != want {
			t.Errorf("NormalizeModelSlug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestModelsForProvider_Unknown(t *testing.T) {
	models := ModelsForProvider("unknown")
	if models != nil {
		t.Errorf("expected nil for unknown provider, got %v", models)
	}
}

func TestModelsForProviderReturnsCopy(t *testing.T) {
	models := ModelsForProvider("codex")
	if len(models) == 0 {
		t.Fatal("expected codex models")
	}

	models[0].Name = "mutated"
	models[0].Capabilities[0] = "mutated"

	fresh := ModelsForProvider("codex")
	if fresh[0].Name != CodexModels[0].Name {
		t.Fatalf("name mutation leaked into registry: got %q, want %q", fresh[0].Name, CodexModels[0].Name)
	}
	if !slices.Equal(fresh[0].Capabilities, CodexModels[0].Capabilities) {
		t.Fatalf("capability mutation leaked into registry: got %v, want %v", fresh[0].Capabilities, CodexModels[0].Capabilities)
	}
}

func TestModelInfoJSONRoundTrip(t *testing.T) {
	original := ModelInfo{
		Slug:         "claude-sonnet-4-6",
		Name:         "Sonnet 4.6",
		Provider:     "claude",
		Capabilities: []string{"code", "reasoning"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ModelInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Slug != original.Slug {
		t.Errorf("Slug = %q, want %q", decoded.Slug, original.Slug)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name = %q, want %q", decoded.Name, original.Name)
	}
	if decoded.Provider != original.Provider {
		t.Errorf("Provider = %q, want %q", decoded.Provider, original.Provider)
	}
	if len(decoded.Capabilities) != len(original.Capabilities) {
		t.Fatalf("Capabilities len = %d, want %d", len(decoded.Capabilities), len(original.Capabilities))
	}
	for i, cap := range decoded.Capabilities {
		if cap != original.Capabilities[i] {
			t.Errorf("Capabilities[%d] = %q, want %q", i, cap, original.Capabilities[i])
		}
	}
}

func TestModelInfoJSONOmitsEmptyCapabilities(t *testing.T) {
	m := ModelInfo{Slug: "test", Name: "Test", Provider: "test"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	if _, present := raw["capabilities"]; present {
		t.Error("expected capabilities to be omitted when nil, but it was present")
	}
}

// TestEveryModelFlagsExactlyOneDefaultContextWindow is the enforcement half of
// the ContextWindowOption.Default contract: the flag, not slice position,
// picks a new thread's tier, so a catalog entry that carries zero flags (or
// two) is a bug even though DefaultContextWindowForOptions would still return
// something usable.
func TestEveryModelFlagsExactlyOneDefaultContextWindow(t *testing.T) {
	catalogs := map[string][]ModelInfo{
		"claude":     ClaudeModels,
		"claude-tui": ClaudeTUIModels,
		"codex":      CodexModels,
	}
	for catalog, models := range catalogs {
		for _, model := range models {
			if len(model.ContextWindows) == 0 {
				t.Errorf("%s/%s advertises no context windows", catalog, model.Slug)
				continue
			}
			flagged := 0
			for _, option := range model.ContextWindows {
				if option.Default {
					flagged++
				}
			}
			if flagged != 1 {
				t.Errorf("%s/%s flags %d default context windows, want exactly 1: %#v",
					catalog, model.Slug, flagged, model.ContextWindows)
			}
		}
	}
}

func TestDefaultContextWindowForOptions(t *testing.T) {
	t.Run("flag wins over position", func(t *testing.T) {
		options := []ContextWindowOption{
			{Tokens: 200000, Tier: ContextTierStandard},
			{Tokens: 1000000, Tier: ContextTierExtended, Default: true},
		}
		tokens, ok := DefaultContextWindowForOptions(options)
		if !ok || tokens != 1000000 {
			t.Fatalf("DefaultContextWindowForOptions = (%d, %v), want (1000000, true)", tokens, ok)
		}
	})

	t.Run("first flag wins when several are set", func(t *testing.T) {
		options := []ContextWindowOption{
			{Tokens: 200000, Default: true},
			{Tokens: 1000000, Default: true},
		}
		if tokens, _ := DefaultContextWindowForOptions(options); tokens != 200000 {
			t.Fatalf("DefaultContextWindowForOptions = %d, want 200000", tokens)
		}
	})

	t.Run("unflagged list falls back to the first element", func(t *testing.T) {
		// Documented fallback: an unflagged list is a catalog bug (caught by
		// TestEveryModelFlagsExactlyOneDefaultContextWindow), and resolving to
		// a real selectable tier beats handing callers a zero-token window.
		options := []ContextWindowOption{
			{Tokens: 272000, Tier: ContextTierStandard},
			{Tokens: 1000000, Tier: ContextTierExtended},
		}
		tokens, ok := DefaultContextWindowForOptions(options)
		if !ok || tokens != 272000 {
			t.Fatalf("DefaultContextWindowForOptions = (%d, %v), want (272000, true)", tokens, ok)
		}
	})

	t.Run("empty list reports no opinion", func(t *testing.T) {
		if tokens, ok := DefaultContextWindowForOptions(nil); ok || tokens != 0 {
			t.Fatalf("DefaultContextWindowForOptions(nil) = (%d, %v), want (0, false)", tokens, ok)
		}
	})
}

// TestClaudeDefaultContextWindowPerModel pins the product decision: the large
// models start new threads on 1M, Sonnet keeps 1M as an opt-in, and Haiku has
// only the one tier.
func TestClaudeDefaultContextWindowPerModel(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-fable-5", ClaudeExtendedContextWindow},
		{"claude-opus-5", ClaudeExtendedContextWindow},
		{"claude-opus-4-8", ClaudeExtendedContextWindow},
		{"claude-opus-4-7", ClaudeExtendedContextWindow},
		{"claude-opus-4-6", ClaudeExtendedContextWindow},
		{"claude-opus-4-5", ClaudeExtendedContextWindow},
		{"claude-sonnet-5", ClaudeStandardContextWindow},
		{"claude-sonnet-4-6", ClaudeStandardContextWindow},
		{"claude-haiku-4-5", ClaudeStandardContextWindow},
	}

	for _, tc := range cases {
		for _, providerName := range []string{string(Claude), string(ClaudeTUI)} {
			t.Run(providerName+"/"+tc.model, func(t *testing.T) {
				if got := DefaultContextWindowForModel(providerName, tc.model, 0); got != tc.want {
					t.Fatalf("DefaultContextWindowForModel = %d, want %d", got, tc.want)
				}
				// The default must also be a tier the model actually offers.
				if !ContextWindowSupportedForModel(providerName, tc.model, tc.want) {
					t.Fatalf("default %d is not an advertised option for %s/%s", tc.want, providerName, tc.model)
				}
			})
		}
	}

	// Aliases resolve through the same catalog entries.
	if got := DefaultContextWindowForModel(string(Claude), "opus", 0); got != ClaudeExtendedContextWindow {
		t.Errorf(`DefaultContextWindowForModel("opus") = %d, want %d`, got, ClaudeExtendedContextWindow)
	}
	if got := DefaultContextWindowForModel(string(Claude), "sonnet", 0); got != ClaudeStandardContextWindow {
		t.Errorf(`DefaultContextWindowForModel("sonnet") = %d, want %d`, got, ClaudeStandardContextWindow)
	}
}

// TestCodexDefaultContextWindowUnchanged guards the blast radius: flipping the
// Claude large-model default must not move Codex's, whose 1M tier stays opt-in.
func TestCodexDefaultContextWindowUnchanged(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-5.6-sol", Codex56ContextWindow},
		{"gpt-5.4", CodexStandardContextWindow},
		{"gpt-5.5", CodexStandardContextWindow},
		{"gpt-5.3-codex-spark", CodexSparkContextWindow},
	}
	for _, tc := range cases {
		if got := DefaultContextWindowForModel(string(Codex), tc.model, 0); got != tc.want {
			t.Errorf("DefaultContextWindowForModel(codex, %q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

// TestResolveContextWindowKeepsStoredChoice is the persisted-thread guard: a
// thread that stored 200k on a now-1M-default model must keep 200k. The
// default only fills in when the stored value is absent or no longer offered.
func TestResolveContextWindowKeepsStoredChoice(t *testing.T) {
	const model = "claude-opus-5"

	if got := ResolveContextWindowForModel(string(Claude), model, ClaudeStandardContextWindow); got != ClaudeStandardContextWindow {
		t.Fatalf("stored 200k on %s resolved to %d, want %d", model, got, ClaudeStandardContextWindow)
	}
	if got := ResolveContextWindowForModel(string(Claude), model, ClaudeExtendedContextWindow); got != ClaudeExtendedContextWindow {
		t.Fatalf("stored 1M on %s resolved to %d, want %d", model, got, ClaudeExtendedContextWindow)
	}
	// Unset (new thread) and retired values fall through to the new default.
	if got := ResolveContextWindowForModel(string(Claude), model, 0); got != ClaudeExtendedContextWindow {
		t.Fatalf("unset window on %s resolved to %d, want %d", model, got, ClaudeExtendedContextWindow)
	}
	if got := ResolveContextWindowForModel(string(Claude), model, 123456); got != ClaudeExtendedContextWindow {
		t.Fatalf("retired window on %s resolved to %d, want %d", model, got, ClaudeExtendedContextWindow)
	}
}

// The KNOWN-WITHOUT-EFFORTS versus UNKNOWN distinction is the whole point of
// ModelDeclaresNoReasoningEffort: only a model we list AND that advertises no
// tiers may have its --effort flag dropped. A model the catalog has never heard
// of keeps its effort, because silence is not a denial.
func TestModelDeclaresNoReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		want     bool
	}{
		{name: "haiku declares none", provider: string(Claude), model: "claude-haiku-4-5", want: true},
		{name: "haiku on claude-tui declares none", provider: string(ClaudeTUI), model: "claude-haiku-4-5", want: true},
		{name: "extended-context haiku alias declares none", provider: string(Claude), model: "claude-haiku-4-5[1m]", want: true},
		{name: "sonnet declares tiers", provider: string(Claude), model: "claude-sonnet-4-6", want: false},
		{name: "unknown claude model is not a denial", provider: string(Claude), model: "claude-nextgen-9", want: false},
		{name: "codex models declare tiers", provider: string(Codex), model: "gpt-5.6-sol", want: false},
		{name: "unknown provider is not a denial", provider: "nope", model: "whatever", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModelDeclaresNoReasoningEffort(tc.provider, tc.model); got != tc.want {
				t.Fatalf("ModelDeclaresNoReasoningEffort(%q, %q) = %v, want %v", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// Coercion answers a different question than ModelDeclaresNoReasoningEffort and
// must keep answering it: threads.reasoning_effort and
// chat_model_profiles.reasoning_effort are NOT NULL with a CHECK over the
// per-provider enum, so a persisted effort can never be empty even for a model
// that ignores it.
func TestCoerceReasoningEffortKeepsALegalEnumForEffortlessModels(t *testing.T) {
	got := CoerceReasoningEffortForModel(string(Claude), "claude-haiku-4-5", EffortLow)
	if !slices.Contains(AllReasoningEfforts, got) {
		t.Fatalf("coerced effort %q is not a legal enum value; the store CHECK would reject it", got)
	}
	if info, found := FindModel(string(Claude), "claude-haiku-4-5"); !found || len(info.ReasoningEfforts) != 0 {
		t.Fatalf("fixture drift: this test only means something while haiku declares no tiers (found=%v)", found)
	}
}

// TestCloneModelInfoDeepCopiesEveryReferenceField is the guard on the one copy
// helper. Every consumer of a catalog mutates what it receives — the Codex
// custom-model template, the Claude probe merge, ClaudeTUIModels' provider
// re-stamp — so a field that escapes the deep copy lets one caller's edit
// rewrite another caller's catalog. The FastModeTier pointer is the field this
// is easiest to get wrong: a struct copy carries the pointer, not the tier.
func TestCloneModelInfoDeepCopiesEveryReferenceField(t *testing.T) {
	source := ModelInfo{
		Slug:             "gpt-test",
		Name:             "GPT Test",
		Provider:         string(Codex),
		Capabilities:     []string{ModelCapabilityFastMode},
		FastModeTier:     &FastModeTier{ID: "priority", Name: "Fast", Description: "1.5x speed"},
		ContextWindows:   []ContextWindowOption{{Tokens: 272000, Label: "272k", Tier: ContextTierStandard, Default: true}},
		ReasoningEfforts: []ReasoningEffortOption{{Slug: "high", Label: "High", Default: true}},
	}

	cloned := CloneModelInfo(source)
	if cloned.FastModeTier == source.FastModeTier {
		t.Fatal("CloneModelInfo shares the FastModeTier pointer with its source")
	}
	if *cloned.FastModeTier != *source.FastModeTier {
		t.Fatalf("cloned FastModeTier = %#v, want %#v", *cloned.FastModeTier, *source.FastModeTier)
	}

	// Mutating every reference-typed field on the copy must leave the source
	// untouched, and vice versa.
	cloned.FastModeTier.ID = "turbo"
	cloned.Capabilities[0] = "mutated"
	cloned.ContextWindows[0].Tokens = 1
	cloned.ReasoningEfforts[0].Slug = "mutated"

	if source.FastModeTier.ID != "priority" {
		t.Errorf("source FastModeTier.ID = %q, want priority — the clone wrote through", source.FastModeTier.ID)
	}
	if source.Capabilities[0] != ModelCapabilityFastMode {
		t.Errorf("source Capabilities = %#v, want untouched", source.Capabilities)
	}
	if source.ContextWindows[0].Tokens != 272000 {
		t.Errorf("source ContextWindows = %#v, want untouched", source.ContextWindows)
	}
	if source.ReasoningEfforts[0].Slug != "high" {
		t.Errorf("source ReasoningEfforts = %#v, want untouched", source.ReasoningEfforts)
	}

	// CloneModels is the list form of the same guarantee — the cache hands its
	// entries out through it on every Get.
	list := CloneModels([]ModelInfo{source})
	if list[0].FastModeTier == source.FastModeTier {
		t.Fatal("CloneModels shares the FastModeTier pointer with its source")
	}
	if CloneModels(nil) != nil {
		t.Error("CloneModels(nil) should stay nil")
	}
}

// TestModelInfoOmitsAnAbsentFastModeTier keeps the wire shape optional: Claude
// declares no tier, and the frontend's fallback to its "Fast" literals is keyed
// on the field being absent rather than an empty object.
func TestModelInfoOmitsAnAbsentFastModeTier(t *testing.T) {
	encoded, err := json.Marshal(ModelInfo{Slug: "claude-opus-5", Name: "Claude Opus 5", Provider: string(Claude)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["fastModeTier"]; present {
		t.Fatalf("encoded ModelInfo carries fastModeTier = %v, want the key omitted", decoded["fastModeTier"])
	}
}

// TestReasoningEffortSetsAreOneVocabulary ties the three lists this package
// publishes together. AllReasoningEfforts is what def, the normalizer, and
// every "is this a known slug" check measure against; the per-provider sets are
// what decides whether a given provider may carry a slug. A tier added to one
// and not the others is the failure this catches: added to a provider set only,
// NormalizeReasoningEffort would coerce it away after validation accepted it;
// added to AllReasoningEfforts only, no provider could ever use it.
func TestReasoningEffortSetsAreOneVocabulary(t *testing.T) {
	perProvider := map[string][]ReasoningEffort{
		string(Claude):    claudeReasoningEfforts,
		string(ClaudeTUI): claudeReasoningEfforts,
		string(Codex):     codexReasoningEfforts,
	}

	union := map[ReasoningEffort]bool{}
	for providerName, efforts := range perProvider {
		seen := map[ReasoningEffort]bool{}
		lastIndex := -1
		for _, effort := range efforts {
			if seen[effort] {
				t.Errorf("%s lists %q twice", providerName, effort)
			}
			seen[effort] = true
			union[effort] = true

			if !slices.Contains(AllReasoningEfforts, effort) {
				t.Errorf("%s declares %q but AllReasoningEfforts does not; the slug would be normalized away after validation accepted it", providerName, effort)
			}
			// The per-provider sets are subsequences of the canonical order, so
			// a picker built from either renders the same low-to-high ordering.
			if index := slices.Index(AllReasoningEfforts, effort); index <= lastIndex {
				t.Errorf("%s lists %q out of canonical order", providerName, effort)
			} else {
				lastIndex = index
			}

			if !providerSupportsReasoningEffort(providerName, string(effort)) {
				t.Errorf("providerSupportsReasoningEffort(%s, %q) = false for a canonical tier", providerName, effort)
			}
		}
		for _, effort := range AllReasoningEfforts {
			if seen[effort] {
				continue
			}
			if providerSupportsReasoningEffort(providerName, string(effort)) {
				t.Errorf("providerSupportsReasoningEffort(%s, %q) = true but the canonical set omits it", providerName, effort)
			}
		}
	}

	for _, effort := range AllReasoningEfforts {
		if !union[effort] {
			t.Errorf("AllReasoningEfforts declares %q but no provider accepts it", effort)
		}
	}

	// An unknown provider is a refusal, not a permissive default.
	if got := ReasoningEffortsForProvider("gemini"); got != nil {
		t.Errorf("ReasoningEffortsForProvider(gemini) = %v, want nil", got)
	}
	if providerSupportsReasoningEffort("gemini", string(EffortHigh)) {
		t.Error("providerSupportsReasoningEffort accepted a tier for an unknown provider")
	}
}

// The Codex catalog's max/ultra tiers are the reason the store CHECK was
// widened (migration v19). Pin that the shipped fallback catalog still names
// them, so a catalog edit cannot quietly take the picker back to xhigh while
// the schema, the validators, and the wire mapping still advertise support.
func TestCodexCatalogOffersTheTopTiersWhereTheyExist(t *testing.T) {
	for slug, want := range map[string][]ReasoningEffort{
		"gpt-5.6-sol":   {EffortMax, EffortUltra},
		"gpt-5.6-terra": {EffortMax, EffortUltra},
		"gpt-5.6-luna":  {EffortMax},
	} {
		info, found := FindModel(string(Codex), slug)
		if !found {
			t.Errorf("codex catalog has no %s", slug)
			continue
		}
		for _, effort := range want {
			if !ModelInfoSupportsReasoningEffort(info, string(effort)) {
				t.Errorf("%s does not offer %q", slug, effort)
			}
		}
	}

	// The conservative default set stays conservative: the older models must
	// not gain tiers nobody has seen them advertise.
	info, found := FindModel(string(Codex), "gpt-5.5")
	if !found {
		t.Fatal("codex catalog has no gpt-5.5")
	}
	for _, effort := range []ReasoningEffort{EffortMax, EffortUltra} {
		if ModelInfoSupportsReasoningEffort(info, string(effort)) {
			t.Errorf("gpt-5.5 advertises %q in the static fallback; only the live catalog may add a tier", effort)
		}
	}
}
