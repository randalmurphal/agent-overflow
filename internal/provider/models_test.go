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
	if models[0].Slug != "gpt-5.5" {
		t.Fatalf("first codex model = %q, want gpt-5.5", models[0].Slug)
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
