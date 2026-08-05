package provider

import (
	"slices"
	"strings"
)

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
	Codex56ContextWindow        = 372000
	CodexExtendedContextWindow  = 1000000
	CodexSparkContextWindow     = 128000
)

// ContextWindowOption describes one selectable context tier for a model.
//
// Default marks the tier a new thread starts on. The flag — never slice
// position — is the contract: `DefaultContextWindowForOptions` reads it, and
// `TestEveryModelFlagsExactlyOneDefaultContextWindow` enforces that every
// catalog entry carries exactly one. That keeps the picker free to reorder
// (or a tier free to be inserted) without silently moving the default.
type ContextWindowOption struct {
	Tokens  int    `json:"tokens"`
	Label   string `json:"label"`
	Tier    string `json:"tier"`
	Default bool   `json:"default,omitempty"`
}

// ReasoningEffortOption describes one selectable reasoning tier for a model.
type ReasoningEffortOption struct {
	Slug    string `json:"slug"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

// FastModeTier is the provider-declared service tier a fast-mode turn runs on.
// Codex reports one per model on `model/list` (`serviceTiers[]`): ID is what
// goes on the wire, Name is what the composer labels the toggle, Description is
// the tier's own blurb ("1.5x speed, increased usage").
//
// Nil means the provider has no tier to name — Claude, whose fast mode is a
// spawn flag with no wire tier, and any Codex catalog entry that predates the
// field. The ModelCapabilityFastMode marker on Capabilities remains the support
// gate for both providers; this only carries the WHICH, never the WHETHER.
type FastModeTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ModelInfo describes a model available from a provider.
type ModelInfo struct {
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	Provider     string        `json:"provider"`
	IsCustom     bool          `json:"isCustom,omitempty"`
	Capabilities []string      `json:"capabilities,omitempty"`
	FastModeTier *FastModeTier `json:"fastModeTier,omitempty"`
	// SupportsAutoMode is deliberately three-state: nil means no source
	// has SAID whether the model can run the Auto runtime mode — the
	// hand-maintained catalog never states it, only the Claude wire does
	// (per-model `supportsAutoMode` on the probe's `initialize` models).
	// Consumers may restrict Auto ONLY on an explicit false; treating
	// nil as false would mis-disable Auto on every model the wire
	// doesn't list.
	SupportsAutoMode *bool                   `json:"supportsAutoMode,omitempty"`
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
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		// Same capability shape as Opus 4.8 — fast-mode capable, 1M
		// extended context ([1m] wire tier), effort low through max
		// including xhigh — per the claude 2.1.219 binary's API
		// capability table (effort/max_effort/xhigh_effort/fast_mode
		// on the claude-opus-5 entry).
		Slug:             "claude-opus-5",
		Name:             "Claude Opus 5",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-8",
		Name:             "Claude Opus 4.8",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-7",
		Name:             "Claude Opus 4.7",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("xhigh", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-6",
		Name:             "Claude Opus 4.6",
		Provider:         "claude",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortMax),
	},
	{
		Slug:             "claude-opus-4-5",
		Name:             "Claude Opus 4.5",
		Provider:         "claude",
		ContextWindows:   claudeContextOptionsDefaultingToExtended(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortMax),
	},
	{
		// Sonnet advertises the 1M tier but keeps 200k as the default:
		// the claude binary treats 1M as an explicit opt-in for Sonnet,
		// and we track that rather than doubling every Sonnet thread's
		// context cost by default.
		Slug:             "claude-sonnet-5",
		Name:             "Claude Sonnet 5",
		Provider:         "claude",
		ContextWindows:   claudeContextOptionsDefaultingToStandard(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		// The claude binary's picker offers xhigh for Sonnet 4.6, but the
		// API capability table lacks xhigh_effort for it — selecting xhigh
		// silently downgrades to high. Expose only efforts the API honors:
		// low→high plus max. Context default is 200k, as on Sonnet 5.
		Slug:             "claude-sonnet-4-6",
		Name:             "Claude Sonnet 4.6",
		Provider:         "claude",
		ContextWindows:   claudeContextOptionsDefaultingToStandard(),
		ReasoningEfforts: claudeEffortOptions("high", EffortLow, EffortMedium, EffortHigh, EffortMax),
	},
	{
		// No reasoning efforts: the CLI's own model list on 2.1.219 reports
		// Haiku with neither `supportsEffort` nor `supportedEffortLevels`,
		// under both subscription and API-key auth (captured in
		// docs/references/fixtures/claude/initialize_models_20260802.json).
		// The catalog used to declare low/medium/high here — the one real
		// catalog-vs-wire discrepancy the 2.7 spike turned up. An empty list
		// is the honest encoding: internal/claudemodels would otherwise report
		// this as drift on every probe, ConfigFromOptions now omits `--effort`
		// for it, and the composer hides the effort section rather than
		// offering tiers the model does not have.
		Slug:           "claude-haiku-4-5",
		Name:           "Claude Haiku 4.5",
		Provider:       "claude",
		ContextWindows: claudeStandardContextOptions(),
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
	out := CloneModels(src)
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
		Slug:             "gpt-5.6-sol",
		Name:             "GPT 5.6 Sol",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codex56ContextOptions(),
		ReasoningEfforts: codexEffortOptions("low", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax, EffortUltra),
	},
	{
		Slug:             "gpt-5.6-terra",
		Name:             "GPT 5.6 Terra",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codex56ContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax, EffortUltra),
	},
	{
		Slug:             "gpt-5.6-luna",
		Name:             "GPT 5.6 Luna",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codex56ContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
	},
	{
		Slug:             "gpt-5.5",
		Name:             "GPT 5.5",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.4",
		Name:             "GPT 5.4",
		Provider:         "codex",
		Capabilities:     []string{ModelCapabilityFastMode},
		ContextWindows:   codexExtendedContextOptions(),
		ReasoningEfforts: codexEffortOptions("xhigh"),
	},
	{
		Slug:             "gpt-5.2",
		Name:             "GPT 5.2",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.4-mini",
		Name:             "GPT 5.4 Mini",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.3-codex",
		Name:             "GPT 5.3 Codex",
		Provider:         "codex",
		ContextWindows:   codexStandardContextOptions(),
		ReasoningEfforts: codexEffortOptions("medium"),
	},
	{
		Slug:             "gpt-5.3-codex-spark",
		Name:             "GPT 5.3 Codex Spark",
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
	return CloneModels(models)
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

// HasContextMarker reports whether a model id carries a trailing `[…]`
// context-tier marker (`claude-sonnet-5[1m]`).
func HasContextMarker(model string) bool {
	model = strings.TrimSpace(model)
	return strings.HasSuffix(model, "]") && strings.LastIndexByte(model, '[') > 0
}

// TrimContextMarker removes a trailing `[…]` context-tier marker from a model
// id. The marker is a CONTEXT TIER, not part of the model identity — AO carries
// the tier on the thread's ContextWindow column and re-appends the marker at
// spawn (claude.claudeModelForContextWindow is the inverse). Marker-carrying
// ids reach us from both directions: the CLI's own model list bakes them into
// id strings, and anything reading a launched model id back sees what we sent.
func TrimContextMarker(model string) string {
	model = strings.TrimSpace(model)
	if !HasContextMarker(model) {
		return model
	}
	return strings.TrimSpace(model[:strings.LastIndexByte(model, '[')])
}

// NormalizeModelSlug resolves the same short aliases t3-code accepts on model
// inputs. It does not validate availability; app-server model/list remains the
// Codex source of truth for live picker contents.
//
// For Claude it also drops a context-tier marker, so a lookup keyed on the slug
// (FindModel and everything built on it — effort tiers, fast-mode support,
// context-window options) cannot silently degrade to "unknown model" just
// because the id arrived on its 1M spelling.
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
		model = TrimContextMarker(model)
		switch model {
		case "fable", "fable-5":
			return "claude-fable-5"
		case "opus", "opus-5":
			return "claude-opus-5"
		case "opus-4.8", "claude-opus-4.8":
			return "claude-opus-4-8"
		case "opus-4.7", "claude-opus-4.7":
			return "claude-opus-4-7"
		case "opus-4.6", "claude-opus-4.6":
			return "claude-opus-4-6"
		case "sonnet", "sonnet-5":
			return "claude-sonnet-5"
		case "sonnet-4.6", "claude-sonnet-4.6":
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

// ModelDeclaresNoReasoningEffort reports whether providerName/model is a
// catalog-KNOWN model that advertises zero reasoning tiers — the signal that an
// effort must not be sent to the CLI at all (Claude's Haiku, per the CLI's own
// model list).
//
// The distinction that matters is KNOWN-WITHOUT-EFFORTS versus UNKNOWN: a model
// the catalog has never heard of — a new one the CLI ships before we list it —
// keeps its effort, because silence is not a denial.
//
// This is deliberately separate from the Coerce* family. Those resolve the
// value that gets PERSISTED, and the threads / chat_model_profiles CHECK
// constraints require a legal enum there, so they can never answer "none".
// Whether the flag is sent is an argv-boundary question, and this is the
// predicate every argv builder asks.
func ModelDeclaresNoReasoningEffort(providerName, model string) bool {
	info, found := FindModel(providerName, model)
	return found && len(info.ReasoningEfforts) == 0
}

func ReasoningEffortSupportedForModel(providerName, model, effort string) bool {
	candidate, found := FindModel(providerName, model)
	if !found {
		return providerSupportsReasoningEffort(providerName, effort)
	}
	return ModelInfoSupportsReasoningEffort(candidate, effort)
}

// ModelInfoSupportsReasoningEffort reports whether model advertises a known
// reasoning-effort slug. The enum check keeps live provider metadata from
// accepting a value that the session configuration cannot preserve.
func ModelInfoSupportsReasoningEffort(model ModelInfo, effort string) bool {
	if !slices.Contains(AllReasoningEfforts, ReasoningEffort(effort)) {
		return false
	}
	for _, option := range model.ReasoningEfforts {
		if option.Slug == effort {
			return true
		}
	}
	return false
}

// CoerceReasoningEffortForModelInfo keeps an advertised effort, otherwise
// choosing the model's advertised default and then its first known option.
func CoerceReasoningEffortForModelInfo(model ModelInfo, effort ReasoningEffort) ReasoningEffort {
	if ModelInfoSupportsReasoningEffort(model, string(effort)) {
		return effort
	}
	for _, option := range model.ReasoningEfforts {
		if option.Default && slices.Contains(AllReasoningEfforts, ReasoningEffort(option.Slug)) {
			return ReasoningEffort(option.Slug)
		}
	}
	for _, option := range model.ReasoningEfforts {
		if slices.Contains(AllReasoningEfforts, ReasoningEffort(option.Slug)) {
			return ReasoningEffort(option.Slug)
		}
	}
	return providerDefaultReasoningEffort(model.Provider)
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
		case string(EffortNone), string(EffortMinimal), string(EffortLow), string(EffortMedium), string(EffortHigh), string(EffortXHigh), string(EffortMax), string(EffortUltra):
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

// DefaultContextWindowForOptions returns the tokens of the option flagged
// Default. The flag is the contract — see ContextWindowOption.
//
// Falling back to the first element when nothing is flagged is deliberate: an
// unflagged list is a catalog bug (caught by
// TestEveryModelFlagsExactlyOneDefaultContextWindow), and resolving it to a
// real, selectable tier keeps a would-be bug from becoming a zero-token
// context window at runtime. Do not read this as "position picks the default";
// flag the option you mean.
//
// ok is false only for an empty list, so callers can distinguish "this model
// has no registry opinion" from "the default happens to be 0".
func DefaultContextWindowForOptions(options []ContextWindowOption) (tokens int, ok bool) {
	for _, option := range options {
		if option.Default {
			return option.Tokens, true
		}
	}
	if len(options) > 0 {
		return options[0].Tokens, true
	}
	return 0, false
}

// DefaultContextWindowForModel resolves the window a new thread starts on for
// a (provider, model) pair. fallback is consulted only when the registry has
// no options at all for the pair.
func DefaultContextWindowForModel(providerName, model string, fallback int) int {
	if tokens, ok := DefaultContextWindowForOptions(ContextWindowOptionsForModel(providerName, model)); ok {
		return tokens
	}
	if fallback > 0 {
		return fallback
	}
	if providerName == string(Codex) {
		return CodexStandardContextWindow
	}
	return ClaudeStandardContextWindow
}

func ResolveContextWindowForModel(providerName, model string, requested int) int {
	if requested > 0 && ContextWindowSupportedForModel(providerName, model, requested) {
		return requested
	}
	return DefaultContextWindowForModel(providerName, model, requested)
}

// CloneModelInfo returns a deep copy of one entry: every reference-typed field
// is reallocated, so the copy and the source share nothing a caller could
// mutate across. This is the ONE place a new slice or pointer field on
// ModelInfo has to be handled — CloneModels and the Codex custom-model template
// both route through it, so neither can silently miss one.
func CloneModelInfo(model ModelInfo) ModelInfo {
	cloned := model
	if len(model.Capabilities) > 0 {
		cloned.Capabilities = append([]string(nil), model.Capabilities...)
	}
	if model.FastModeTier != nil {
		tier := *model.FastModeTier
		cloned.FastModeTier = &tier
	}
	if model.SupportsAutoMode != nil {
		supports := *model.SupportsAutoMode
		cloned.SupportsAutoMode = &supports
	}
	if len(model.ContextWindows) > 0 {
		cloned.ContextWindows = append([]ContextWindowOption(nil), model.ContextWindows...)
	}
	if len(model.ReasoningEfforts) > 0 {
		cloned.ReasoningEfforts = append([]ReasoningEffortOption(nil), model.ReasoningEfforts...)
	}
	return cloned
}

// CloneModels returns a deep copy of a model list: the slice, plus every
// reference-typed field on every entry. The one copy helper for a type whose
// consumers (the live Codex catalog, the probe-enriched Claude catalog, every
// ModelsForProvider caller) all mutate what they receive.
func CloneModels(models []ModelInfo) []ModelInfo {
	if models == nil {
		return nil
	}
	cloned := make([]ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = CloneModelInfo(model)
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

func codexEffortOptions(defaultSlug string, efforts ...ReasoningEffort) []ReasoningEffortOption {
	if len(efforts) == 0 {
		efforts = []ReasoningEffort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	}
	options := make([]ReasoningEffortOption, 0, len(efforts))
	for _, effort := range efforts {
		slug := string(effort)
		options = append(options, NewReasoningEffortOption(slug, slug == defaultSlug))
	}
	return options
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
	case "ultra":
		return "Ultra"
	default:
		return slug
	}
}

func claudeStandardContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens:  ClaudeStandardContextWindow,
		Label:   "200k",
		Tier:    ContextTierStandard,
		Default: true,
	}}
}

// claudeExtendedContextOptions builds the 200k/1m pair every Claude model that
// speaks the `[1m]` wire tier offers. defaultExtended chooses which tier a new
// thread starts on; call it through one of the two named wrappers below rather
// than passing a bare bool at the catalog entry.
func claudeExtendedContextOptions(defaultExtended bool) []ContextWindowOption {
	return []ContextWindowOption{
		{Tokens: ClaudeStandardContextWindow, Label: "200k", Tier: ContextTierStandard, Default: !defaultExtended},
		{Tokens: ClaudeExtendedContextWindow, Label: "1m", Tier: ContextTierExtended, Default: defaultExtended},
	}
}

// claudeContextOptionsDefaultingToExtended: new threads start on the 1M tier.
// Used by the large models (Fable 5, the Opus family), where the extra context
// is the point of the model and the cost tradeoff is one the user opted into
// by picking it.
func claudeContextOptionsDefaultingToExtended() []ContextWindowOption {
	return claudeExtendedContextOptions(true)
}

// claudeContextOptionsDefaultingToStandard: new threads start on 200k and 1M
// stays opt-in. Used by the Sonnet tier, matching how the claude binary itself
// treats 1M for Sonnet.
func claudeContextOptionsDefaultingToStandard() []ContextWindowOption {
	return claudeExtendedContextOptions(false)
}

func codexStandardContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens:  CodexStandardContextWindow,
		Label:   "272k",
		Tier:    ContextTierStandard,
		Default: true,
	}}
}

func codex56ContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens:  Codex56ContextWindow,
		Label:   "372k",
		Tier:    ContextTierStandard,
		Default: true,
	}}
}

// codexExtendedContextOptions keeps 272k as the default: the 1M tier is a
// Codex-side opt-in, unchanged by the Claude large-model default flip.
func codexExtendedContextOptions() []ContextWindowOption {
	return []ContextWindowOption{
		{Tokens: CodexStandardContextWindow, Label: "272k", Tier: ContextTierStandard, Default: true},
		{Tokens: CodexExtendedContextWindow, Label: "1m", Tier: ContextTierExtended},
	}
}

func codexSparkContextOptions() []ContextWindowOption {
	return []ContextWindowOption{{
		Tokens:  CodexSparkContextWindow,
		Label:   "128k",
		Tier:    ContextTierStandard,
		Default: true,
	}}
}
