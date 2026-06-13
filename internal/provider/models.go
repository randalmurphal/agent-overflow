package provider

const (
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
	CodexExtendedContextWindow  = 1000000
	CodexSparkContextWindow     = 128000
)

// ContextWindowOption describes one selectable context tier for a model.
type ContextWindowOption struct {
	Tokens int    `json:"tokens"`
	Label  string `json:"label"`
	Tier   string `json:"tier"`
}

// ReasoningEffortOption describes one selectable reasoning tier for a model.
type ReasoningEffortOption struct {
	Slug    string `json:"slug"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

// ModelInfo describes a model available from a provider.
type ModelInfo struct {
	Slug             string                  `json:"slug"`
	Name             string                  `json:"name"`
	Provider         string                  `json:"provider"`
	IsCustom         bool                    `json:"isCustom,omitempty"`
	Capabilities     []string                `json:"capabilities,omitempty"`
	ContextWindows   []ContextWindowOption   `json:"contextWindows,omitempty"`
	ReasoningEfforts []ReasoningEffortOption `json:"reasoningEfforts,omitempty"`
}

// ClaudeModels lists models available through the Claude provider.
var ClaudeModels = []ModelInfo{
	{
		// Fable 5 is the top tier above Opus. Same launch surface as
		// Opus 4.8 (1M context, low→max effort, xhigh default). Fast
		// mode is an Opus-only feature, so it is intentionally absent
		// here. Listed first, so it is the FallbackModelForProvider
		// default for new threads.
		Slug:             "claude-fable-5",
		Name:             "Claude Fable 5",
		Provider:         "claude",
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-8",
		Name:             "Claude Opus 4.8",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-7",
		Name:             "Claude Opus 4.7",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-6",
		Name:             "Claude Opus 4.6",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-5",
		Name:             "Claude Opus 4.5",
		Provider:         "claude",
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortMax),
	},
	{
		Slug:             "claude-sonnet-4-6",
		Name:             "Claude Sonnet 4.6",
		Provider:         "claude",
		ContextWindows:   claudeExtendedContextOptions(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh),
	},
	{
		Slug:             "claude-haiku-4-5",
		Name:             "Claude Haiku 4.5",
		Provider:         "claude",
		ContextWindows:   claudeStandardContextOptions(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh),
	},
}

// ClaudeTUIModels mirrors ClaudeModels: the interactive Claude Code TUI runs
// the same claude binary, so it exposes the identical model catalog. Each
// entry's Provider is stamped claude-tui so favorites and provider/model
// round-trips resolve to the right provider. Built once from ClaudeModels;
// Go's package-var dependency ordering guarantees ClaudeModels is initialized
// first.
var ClaudeTUIModels = withProvider(ClaudeModels, string(ClaudeTUI))

// withProvider clones a model list and stamps every entry with providerName,
// leaving the source slice untouched.
func withProvider(src []ModelInfo, providerName string) []ModelInfo {
	out := cloneModels(src)
	for i := range out {
		out[i].Provider = providerName
	}
	return out
}

// CodexModels lists models available through the Codex provider.
//
// Codex's live picker list comes from app-server model/list. This slice is
// only the built-in fallback for offline defaults, tests, and stale settings
// normalization. Keep it to current Codex-family models; don't add unrelated
// OpenAI API models here.
var CodexModels = []ModelInfo{
	{
		Slug:             "gpt-5.5",
		Name:             "GPT-5.5",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.4",
		Name:             "GPT-5.4",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codexExtendedContextOptions(),
		ReasoningEfforts: codexEffortOptions("xhigh"),
	},
	{
		Slug:             "gpt-5.2",
		Name:             "GPT-5.2",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.4-mini",
		Name:             "GPT-5.4 Mini",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.3-codex",
		Name:             "GPT-5.3 Codex",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.3-codex-spark",
		Name:             "GPT-5.3 Codex Spark",
		Provider:         "codex",
		ContextWindows:   codexSparkContextOptions(),
		ReasoningEfforts: codexEffortOptions("high"),
	},
}

// ModelsForProvider returns the model list for the given provider name.
// Returns nil for unknown providers.
func ModelsForProvider(providerName string) []ModelInfo {
	models := staticModelsForProvider(providerName)
	if models == nil {
		return nil
	}
	return cloneModels(models)
}

func staticModelsForProvider(providerName string) []ModelInfo {
	switch providerName {
	case string(Claude):
		return ClaudeModels
	case string(ClaudeTUI):
		return ClaudeTUIModels
	case string(Codex):
		return CodexModels
	default:
		return nil
	}
}

// NormalizeModelSlug resolves the same short aliases t3-code accepts on model
// inputs. It does not validate availability; app-server model/list remains the
// Codex source of truth for live picker contents.
func NormalizeModelSlug(providerName, model string) string {
	switch providerName {
	case string(Codex):
		switch model {
		case "gpt-5-codex", "5.4":
			return "gpt-5.4"
		case "5.3", "gpt-5.3":
			return "gpt-5.3-codex"
		case "5.3-spark", "gpt-5.3-spark":
			return "gpt-5.3-codex-spark"
		default:
			return model
		}
	case string(Claude), string(ClaudeTUI):
		switch model {
		case "fable", "fable-5":
			return "claude-fable-5"
		case "opus", "opus-4.8", "claude-opus-4.8":
			return "claude-opus-4-8"
		case "opus-4.7", "claude-opus-4.7":
			return "claude-opus-4-7"
		case "opus-4.6", "claude-opus-4.6":
			return "claude-opus-4-6"
		case "sonnet", "sonnet-4.6", "claude-sonnet-4.6":
			return "claude-sonnet-4-6"
		case "haiku", "haiku-4.5", "claude-haiku-4.5", "claude-haiku-4-5-20251001":
			return "claude-haiku-4-5"
		default:
			return model
		}
	default:
		return model
	}
}

// ContextWindowOptionsForModel returns the selectable context windows for a
// provider/model pair. The returned slice is owned by the caller.
func ContextWindowOptionsForModel(providerName, model string) []ContextWindowOption {
	if candidate, ok := FindModel(providerName, model); ok {
		return append([]ContextWindowOption(nil), candidate.ContextWindows...)
	}
	return nil
}

func ModelSupportsCapability(providerName, model, capability string) bool {
	if candidate, ok := FindModel(providerName, model); ok {
		for _, existing := range candidate.Capabilities {
			if existing == capability {
				return true
			}
		}
	}
	return false
}

func ReasoningEffortOptionsForModel(providerName, model string) []ReasoningEffortOption {
	if candidate, ok := FindModel(providerName, model); ok {
		return append([]ReasoningEffortOption(nil), candidate.ReasoningEfforts...)
	}
	return nil
}

func ReasoningEffortSupportedForModel(providerName, model, effort string) bool {
	if _, found := FindModel(providerName, model); !found {
		return providerSupportsReasoningEffort(providerName, effort)
	}
	options := ReasoningEffortOptionsForModel(providerName, model)
	for _, option := range options {
		if option.Slug == effort {
			return true
		}
	}
	return false
}

func DefaultReasoningEffortForModel(providerName, model string, fallback ReasoningEffort) ReasoningEffort {
	if _, found := FindModel(providerName, model); !found {
		if providerSupportsReasoningEffort(providerName, string(fallback)) {
			return fallback
		}
		return providerDefaultReasoningEffort(providerName)
	}
	options := ReasoningEffortOptionsForModel(providerName, model)
	for _, option := range options {
		if option.Default {
			return ReasoningEffort(option.Slug)
		}
	}
	if len(options) > 0 {
		return ReasoningEffort(options[0].Slug)
	}
	return providerDefaultReasoningEffort(providerName)
}

func providerDefaultReasoningEffort(providerName string) ReasoningEffort {
	switch providerName {
	case string(Codex):
		return EffortHigh
	case string(Claude), string(ClaudeTUI):
		return EffortHigh
	default:
		return DefaultReasoningEffort
	}
}

func FindModel(providerName, model string) (ModelInfo, bool) {
	model = NormalizeModelSlug(providerName, model)
	for _, candidate := range staticModelsForProvider(providerName) {
		if candidate.Slug == model {
			return candidate, true
		}
	}
	return ModelInfo{}, false
}

func CoerceReasoningEffortForModel(providerName, model string, effort ReasoningEffort) ReasoningEffort {
	if ReasoningEffortSupportedForModel(providerName, model, string(effort)) {
		return effort
	}
	return DefaultReasoningEffortForModel(providerName, model, DefaultReasoningEffort)
}

func providerSupportsReasoningEffort(providerName, effort string) bool {
	switch providerName {
	case string(Codex):
		switch effort {
		case string(EffortNone), string(EffortMinimal), string(EffortLow), string(EffortMedium), string(EffortHigh), string(EffortXHigh):
			return true
		default:
			return false
		}
	case string(Claude), string(ClaudeTUI):
		switch effort {
		case string(EffortLow), string(EffortMedium), string(EffortHigh), string(EffortXHigh), string(EffortMax):
			return true
		default:
			return false
		}
	default:
		return false
	}
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
		if len(model.ReasoningEfforts) > 0 {
			cloned[i].ReasoningEfforts = append([]ReasoningEffortOption(nil), model.ReasoningEfforts...)
		}
	}
	return cloned
}

func NewReasoningEffortOption(slug string, isDefault bool) ReasoningEffortOption {
	return NewReasoningEffortOptionWithLabel(slug, "", isDefault)
}

func NewReasoningEffortOptionWithLabel(slug, label string, isDefault bool) ReasoningEffortOption {
	if label == "" {
		label = effortLabel(slug)
	}
	return ReasoningEffortOption{
		Slug:    slug,
		Label:   label,
		Default: isDefault,
	}
}

func claudeEffortOptions(defaultSlug string, efforts ...ReasoningEffort) []ReasoningEffortOption {
	options := make([]ReasoningEffortOption, 0, len(efforts))
	for _, effort := range efforts {
		slug := string(effort)
		options = append(options, NewReasoningEffortOption(slug, slug == defaultSlug))
	}
	return options
}

func codexEffortOptions(defaultSlug string) []ReasoningEffortOption {
	return []ReasoningEffortOption{
		{Slug: "low", Label: "Low", Default: defaultSlug == "low"},
		{Slug: "medium", Label: "Medium", Default: defaultSlug == "medium"},
		{Slug: "high", Label: "High", Default: defaultSlug == "high"},
		{Slug: "xhigh", Label: "xHigh", Default: defaultSlug == "xhigh"},
	}
}

func effortLabel(slug string) string {
	switch slug {
	case "none":
		return "None"
	case "minimal":
		return "Minimal"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "xHigh"
	case "max":
		return "Max"
	default:
		return slug
	}
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

func codexSparkContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens: CodexSparkContextWindow,
		Label:  "128k",
		Tier:   ContextTierStandard,
	}}
}
