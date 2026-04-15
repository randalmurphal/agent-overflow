package provider

import (
	"encoding/json"
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
	}
}

func TestModelsForProvider_Unknown(t *testing.T) {
	models := ModelsForProvider("unknown")
	if models != nil {
		t.Errorf("expected nil for unknown provider, got %v", models)
	}
}

func TestModelInfoJSONRoundTrip(t *testing.T) {
	original := ModelInfo{
		Slug:         "claude-sonnet-4-6",
		Name:         "Claude Sonnet 4.6",
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
