package provider

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
	{Slug: "claude-opus-4-6", Name: "Claude Opus 4.6", Provider: "claude"},
	{Slug: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Provider: "claude"},
}

// CodexModels lists models available through the Codex provider.
var CodexModels = []ModelInfo{
	{Slug: "gpt-5.4", Name: "GPT-5.4", Provider: "codex"},
	{Slug: "gpt-5.4-mini", Name: "GPT-5.4 Mini", Provider: "codex"},
	{Slug: "o3", Name: "o3", Provider: "codex"},
	{Slug: "o4-mini", Name: "o4-mini", Provider: "codex"},
}

// ModelsForProvider returns the model list for the given provider name.
// Returns nil for unknown providers.
func ModelsForProvider(providerName string) []ModelInfo {
	switch providerName {
	case "claude":
		return ClaudeModels
	case "codex":
		return CodexModels
	default:
		return nil
	}
}
