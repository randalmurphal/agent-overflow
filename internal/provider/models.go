package provider

const (
	// ModelCapabilityThinking indicates a model supports an explicit thinking toggle.
	ModelCapabilityThinking = "thinking"
	// ModelCapabilityFastMode indicates a model supports fast-mode execution.
	ModelCapabilityFastMode = "fast_mode"
)

const (
	ContextTierStandard = "standard"
	ContextTierExtended = "extended"
)

const (
	ClaudeStandardContextWindow = 200000
	ClaudeExtendedContextWindow = 1000000
	CodexStandardContextWindow  = 272000
	CodexExtendedContextWindow  = 1050000
)

// ContextWindowOption describes one selectable context tier for a model.
type ContextWindowOption struct {
	Tokens int    `json:"tokens"`
	Label  string `json:"label"`
	Tier   string `json:"tier"`
}

// ModelInfo describes a model available from a provider.
type ModelInfo struct {
	Slug           string                `json:"slug"`
	Name           string                `json:"name"`
	Provider       string                `json:"provider"`
	Capabilities   []string              `json:"capabilities,omitempty"`
	ContextWindows []ContextWindowOption `json:"contextWindows,omitempty"`
}

// ClaudeModels lists models available through the Claude provider.
var ClaudeModels = []ModelInfo{
	{
		Slug:           "claude-sonnet-4-6",
		Name:           "Sonnet 4.6",
		Provider:       "claude",
		ContextWindows: claudeExtendedContextOptions(),
	},
	{
		Slug:           "claude-opus-4-7",
		Name:           "Opus 4.7",
		Provider:       "claude",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: claudeExtendedContextOptions(),
	},
	{
		Slug:           "claude-haiku-4-5",
		Name:           "Haiku 4.5",
		Provider:       "claude",
		Capabilities:   []string{ModelCapabilityThinking},
		ContextWindows: claudeStandardContextOptions(),
	},
}

// CodexModels lists models available through the Codex provider.
var CodexModels = []ModelInfo{
	{
		Slug:           "gpt-5.5",
		Name:           "GPT-5.5",
		Provider:       "codex",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: codexExtendedContextOptions(),
	},
	{
		Slug:           "gpt-5.4",
		Name:           "GPT-5.4",
		Provider:       "codex",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: codexExtendedContextOptions(),
	},
	{
		Slug:           "gpt-5.4-mini",
		Name:           "GPT-5.4 Mini",
		Provider:       "codex",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: codexStandardContextOptions(),
	},
	{
		Slug:           "o3",
		Name:           "o3",
		Provider:       "codex",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: codexStandardContextOptions(),
	},
	{
		Slug:           "o4-mini",
		Name:           "o4-mini",
		Provider:       "codex",
		Capabilities:   []string{ModelCapabilityFastMode},
		ContextWindows: codexStandardContextOptions(),
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

// ContextWindowOptionsForModel returns the selectable context windows for a
// provider/model pair. The returned slice is owned by the caller.
func ContextWindowOptionsForModel(providerName, model string) []ContextWindowOption {
	for _, candidate := range ModelsForProvider(providerName) {
		if candidate.Slug == model {
			return append([]ContextWindowOption(nil), candidate.ContextWindows...)
		}
	}
	return nil
}

func ContextWindowSupportedForModel(providerName, model string, tokens int) bool {
	for _, option := range ContextWindowOptionsForModel(providerName, model) {
		if option.Tokens == tokens {
			return true
		}
	}
	return false
}

func ContextTierForModelWindow(providerName, model string, tokens int) string {
	for _, option := range ContextWindowOptionsForModel(providerName, model) {
		if option.Tokens == tokens {
			return option.Tier
		}
	}
	return ContextTierStandard
}

func DefaultContextWindowForModel(providerName, model string, fallback int) int {
	options := ContextWindowOptionsForModel(providerName, model)
	if len(options) == 0 {
		if fallback > 0 {
			return fallback
		}
		if providerName == "codex" {
			return CodexStandardContextWindow
		}
		return ClaudeStandardContextWindow
	}
	if providerName == "claude" && model == "claude-opus-4-7" {
		for _, option := range options {
			if option.Tier == ContextTierExtended {
				return option.Tokens
			}
		}
	}
	return options[0].Tokens
}

func ResolveContextWindowForModel(providerName, model string, requested int) int {
	if requested > 0 && ContextWindowSupportedForModel(providerName, model, requested) {
		return requested
	}
	return DefaultContextWindowForModel(providerName, model, requested)
}

func cloneModels(models []ModelInfo) []ModelInfo {
	cloned := make([]ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = model
		if len(model.Capabilities) > 0 {
			cloned[i].Capabilities = append([]string(nil), model.Capabilities...)
		}
		if len(model.ContextWindows) > 0 {
			cloned[i].ContextWindows = append([]ContextWindowOption(nil), model.ContextWindows...)
		}
	}
	return cloned
}

func claudeStandardContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens: ClaudeStandardContextWindow,
		Label:  "200k",
		Tier:   ContextTierStandard,
	}}
}

func claudeExtendedContextOptions() []ContextWindowOption {
	return []ContextWindowOption{
		{Tokens: ClaudeStandardContextWindow, Label: "200k", Tier: ContextTierStandard},
		{Tokens: ClaudeExtendedContextWindow, Label: "1m", Tier: ContextTierExtended},
	}
}

func codexStandardContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens: CodexStandardContextWindow,
		Label:  "272k",
		Tier:   ContextTierStandard,
	}}
}

func codexExtendedContextOptions() []ContextWindowOption {
	return []ContextWindowOption{
		{Tokens: CodexStandardContextWindow, Label: "272k", Tier: ContextTierStandard},
		{Tokens: CodexExtendedContextWindow, Label: "1m", Tier: ContextTierExtended},
	}
}
