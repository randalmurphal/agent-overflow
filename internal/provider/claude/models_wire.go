package claude

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// WireModel is one row of the `models` array the CLI returns on the
// `initialize` control_response (and, identically, on `list_models`).
//
// Captured from claude 2.1.219 —
// `docs/references/fixtures/claude/initialize_models_20260802.json`, and the
// field docs below are the CLI's own zod `.describe()` strings read out of the
// binary. This is a PICKER SHORTLIST, not a catalog: it carries the five rows
// the CLI's own model menu shows, with aliases and canonical ids mixed in one
// `value` space, and it omits both older models and every context window. See
// `internal/claudemodels` for what that means for the merge.
//
// Rows are wire input: every consumer must treat unknown or missing fields as
// "the CLI did not say", never as a negative fact about a model the row does
// not mention.
type WireModel struct {
	// Value is the identifier to use in API calls — an alias ("sonnet",
	// "opus[1m]"), the CLI's default pointer ("default"), or a canonical id.
	Value string `json:"value"`
	// ResolvedModel is the canonical wire model id Value resolves to. Optional
	// on the wire; it is itself inconsistent about the `[1m]` context marker
	// (`opus[1m]` resolves to `claude-opus-5[1m]`, but `claude-fable-5[1m]`
	// resolves to the bare `claude-fable-5`), which is why CanonicalSlug strips
	// the marker rather than trusting either side.
	ResolvedModel string `json:"resolvedModel"`
	// DisplayName names the ROW, not the model: "Default (recommended)" and
	// "Opus (1M context)" are two rows for one model.
	DisplayName string `json:"displayName"`
	// Description is prose for the CLI's picker ("Sonnet 5 · Efficient for
	// routine tasks · $2/$10 per Mtok"). Content varies by auth mode — the
	// API-key capture carries per-Mtok pricing the subscription capture omits.
	Description string `json:"description"`

	SupportsEffort           bool     `json:"supportsEffort"`
	SupportedEffortLevels    []string `json:"supportedEffortLevels"`
	SupportsAdaptiveThinking bool     `json:"supportsAdaptiveThinking"`
	SupportsFastMode         bool     `json:"supportsFastMode"`
	// SupportsAutoMode reports whether the model can run under
	// `--permission-mode auto`. Decoded but deliberately not merged into
	// ModelInfo.Capabilities — see internal/claudemodels/AGENTS.md.
	SupportsAutoMode bool `json:"supportsAutoMode"`
	// Disabled marks a model the CLI shows but refuses to select (an org's
	// Zero Data Retention setting excluding it, per the binary's own field
	// doc; the human-readable reason is folded into Description). Decoded and
	// surfaced as drift, never acted on — no capture has ever carried it.
	Disabled bool `json:"disabled"`
	// PromoListPrice is the struck-through pre-promo price ("$3/$15") for a
	// model on a launch promo. Display-only; AO has no pricing surface fed
	// from this wire.
	PromoListPrice string `json:"promoListPrice"`
}

// CanonicalSlug maps a wire row onto the model-slug space the rest of AO uses
// (`provider.ClaudeModels` slugs, thread rows, favorites).
//
// Three normalizations, in order, each one the wire forcing our hand:
//
//  1. Prefer ResolvedModel over Value — Value is an alias space that includes
//     the CLI's own "default" pointer, so it is not a model identity.
//  2. provider.NormalizeModelSlug drops the trailing `[1m]` context marker —
//     which the wire is inconsistent about carrying — and folds the alias
//     spellings and dated ids (`claude-haiku-4-5-20251001` →
//     `claude-haiku-4-5`) onto catalog slugs.
//
// Returns "" for a row that names nothing.
func (m WireModel) CanonicalSlug() string {
	raw := strings.TrimSpace(m.ResolvedModel)
	if raw == "" {
		raw = strings.TrimSpace(m.Value)
	}
	return provider.NormalizeModelSlug(string(provider.Claude), raw)
}

// IsDefaultPointer reports whether the row is the CLI's "default" selection
// pointer rather than a model row. Its DisplayName ("Default (recommended)")
// names the pointer, so it must never become a model's name.
func (m WireModel) IsDefaultPointer() bool {
	return strings.TrimSpace(m.Value) == "default"
}

// DeclaresExtendedContext reports whether the row itself proves the model can
// run the 1M tier, by carrying the `[1m]` marker on either identifier. Only
// PRESENCE is evidence: `claude-fable-5[1m]` resolves to a marker-less
// `claude-fable-5`, so absence proves nothing.
func (m WireModel) DeclaresExtendedContext() bool {
	return provider.HasContextMarker(m.Value) || provider.HasContextMarker(m.ResolvedModel)
}

// decodeWireModels pulls the `models` array out of the initialize response's
// inner payload. A missing array decodes to nil, which is a real answer (an
// older CLI that does not report models) and not an error — callers must not
// treat nil as "keep whatever you had".
func decodeWireModels(payload json.RawMessage) ([]WireModel, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var inner struct {
		Models []WireModel `json:"models"`
	}
	if err := json.Unmarshal(payload, &inner); err != nil {
		return nil, err
	}
	return inner.Models, nil
}
