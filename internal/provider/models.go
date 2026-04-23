package provider

const (
	// ModelCapabilityThinking indicates a model supports an explicit thinking toggle.
	ModelCapabilityThinking = "thinking"
	// ModelCapabilityFastMode indicates a model supports fast-mode execution.
	ModelCapabilityFastMode = "fast_mode"
)

// ModelInfo describes a model available from a provider.
type ModelInfo struct {
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// ClaudeModels lists models available through the Claude provider.
var ClaudeModels = []ModelInfo{
	{Slug: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Provider: "claude"},
	{
		Slug:         "claude-opus-4-7",
		Name:         "Claude Opus 4.7",
		Provider:     "claude",
		Capabilities: []string{ModelCapabilityFastMode},
	},
	{
		Slug:         "claude-haiku-4-5",
		Name:         "Claude Haiku 4.5",
		Provider:     "claude",
		Capabilities: []string{ModelCapabilityThinking},
	},
}

// CodexModels lists models available through the Codex provider.
var CodexModels = []ModelInfo{
	{
		Slug:         "gpt-5.5",
		Name:         "GPT-5.5",
		Provider:     "codex",
		Capabilities: []string{ModelCapabilityFastMode},
	},
	{
		Slug:         "gpt-5.4",
		Name:         "GPT-5.4",
		Provider:     "codex",
		Capabilities: []string{ModelCapabilityFastMode},
	},
	{
		Slug:         "gpt-5.4-mini",
		Name:         "GPT-5.4 Mini",
		Provider:     "codex",
		Capabilities: []string{ModelCapabilityFastMode},
	},
	{
		Slug:         "o3",
		Name:         "o3",
		Provider:     "codex",
		Capabilities: []string{ModelCapabilityFastMode},
	},
	{
		Slug:         "o4-mini",
		Name:         "o4-mini",
		Provider:     "codex",
		Capabilities: []string{ModelCapabilityFastMode},
	},
}

// ModelsForProvider returns the model list for the given provider name.
// Returns nil for unknown providers.
func ModelsForProvider(providerName string) []ModelInfo {
	switch providerName {
	case "claude":
		return cloneModels(ClaudeModels)
	case "codex":
		return cloneModels(CodexModels)
	default:
		return nil
	}
}

func cloneModels(models []ModelInfo) []ModelInfo {
	cloned := make([]ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = model
		if len(model.Capabilities) > 0 {
			cloned[i].Capabilities = append([]string(nil), model.Capabilities...)
		}
	}
	return cloned
}
