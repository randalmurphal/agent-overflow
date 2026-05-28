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

func TestCodexModelsIncludeGPT55(t *testing.T) {
	models := ModelsForProvider("codex")
	if len(models) == 0 {
		t.Fatal("expected codex models")
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
		{"claude-opus-4-8", true, 2},
		{"claude-opus-4-7", true, 2},
		{"claude-opus-4-6", true, 2},
		{"claude-opus-4-5", false, 2},
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
			if model.ContextWindows[0].Tokens != ClaudeStandardContextWindow {
				t.Fatalf("default ContextWindow = %d, want %d", model.ContextWindows[0].Tokens, ClaudeStandardContextWindow)
			}
		})
	}
}

func TestCodexModelCapabilitiesAndContextWindows(t *testing.T) {
	cases := []struct {
		model    string
		fastMode bool
		windows  []int
		def      string
	}{
		{"gpt-5.4", true, []int{CodexStandardContextWindow, CodexExtendedContextWindow}, "xhigh"},
		{"gpt-5.5", true, []int{CodexStandardContextWindow}, "medium"},
		{"gpt-5.2", false, []int{CodexStandardContextWindow}, "medium"},
		{"gpt-5.3-codex", false, []int{CodexStandardContextWindow}, "medium"},
		{"gpt-5.4-mini", false, []int{CodexStandardContextWindow}, "medium"},
		{"gpt-5.3-codex-spark", false, []int{CodexSparkContextWindow}, "high"},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			model, found := FindModel("codex", tc.model)
			if !found {
				t.Fatalf("FindModel(%q) not found", tc.model)
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

func TestNormalizeModelSlugClaudeAliases(t *testing.T) {
	tests := map[string]string{
		"opus":                       "claude-opus-4-8",
		"claude-opus-4.8":            "claude-opus-4-8",
		"opus-4.7":                   "claude-opus-4-7",
		"claude-opus-4.7":            "claude-opus-4-7",
		"claude-opus-4.6":            "claude-opus-4-6",
		"sonnet":                     "claude-sonnet-4-6",
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
