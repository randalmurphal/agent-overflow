// Package claudemodels merges the model rows the Claude CLI reports on its
// zero-token `initialize` probe into AO's hand-maintained Claude catalog, and
// holds the merged result per probe identity.
//
// The wire list is an ENRICHMENT SOURCE, not a catalog. See AGENTS.md in this
// directory for the merge policy and the evidence behind each rule.
package claudemodels

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// DriftKind classifies one disagreement between the hand catalog and the wire,
// or one place the merge had to fall back. Every kind is a maintenance signal
// for a human reading the log — never a user-facing error, and never a reason
// to drop a model.
type DriftKind string

const (
	// DriftCapability — the catalog's capability flags for a model the wire
	// also lists disagree with the wire. The wire won; fix the catalog.
	DriftCapability DriftKind = "capability"
	// DriftEffort — the catalog's reasoning-effort options disagree with the
	// wire's. The wire won; fix the catalog.
	DriftEffort DriftKind = "effort"
	// DriftFamilyDefault — the wire lists a model the catalog does not know,
	// so its context windows were resolved by family fallback (or defaulted to
	// standard-only). Add it to the catalog to get real windows.
	DriftFamilyDefault DriftKind = "family-default"
	// DriftDisabled — the wire marks a model disabled (an org policy excluding
	// it). Reported only: no capture has ever carried the field, so acting on
	// it could hide a working model on a decode mistake.
	DriftDisabled DriftKind = "disabled"
	// DriftRowConflict — two wire rows resolving to the same model disagree
	// about its capabilities. The first row wins.
	DriftRowConflict DriftKind = "row-conflict"
	// DriftUnreadable — the wire carried a `models` array that would not
	// decode. Nothing was merged and the previous answer was kept.
	DriftUnreadable DriftKind = "unreadable"
)

// Drift is one line of the maintenance report a merge produces.
type Drift struct {
	Model  string
	Kind   DriftKind
	Detail string
}

func (d Drift) String() string {
	if d.Model == "" {
		return fmt.Sprintf("%s: %s", d.Kind, d.Detail)
	}
	return fmt.Sprintf("%s [%s]: %s", d.Model, d.Kind, d.Detail)
}

// FormatDrift renders a drift report as one log line. Empty for no drift, so
// callers can use emptiness as "nothing to say".
func FormatDrift(drift []Drift) string {
	if len(drift) == 0 {
		return ""
	}
	parts := make([]string, 0, len(drift))
	for _, d := range drift {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "; ")
}

// Merge folds the CLI's wire rows into base and returns the picker catalog.
//
// The invariants, in priority order:
//
//  1. **Nothing in base is ever dropped or reordered.** The wire is a
//     five-row picker shortlist that omits still-usable older models
//     (opus-4.x, sonnet-4-6 are absent on 2.1.219), so wire ABSENCE carries no
//     information. Base order also decides the fallback model for new threads.
//  2. **Base owns context windows, and no row's `displayName` ever becomes a
//     model name.** The wire reports no windows at all, and its `displayName`
//     names a picker ROW, not a model — "Default (recommended)" and "Opus (1M
//     context)" are two rows for one model, so adopting them would rename
//     models arbitrarily. A wire-only model is named from its slug instead
//     (newWireOnlyModel).
//  3. **The wire owns capability flags** for models it lists: fast-mode
//     support and the reasoning-effort set. It is the running binary's own
//     answer; a catalog that disagrees is stale, and the disagreement is
//     reported as drift so it gets fixed at the source.
//  4. **Wire-only models are added**, named from their slug and with context
//     windows resolved by family fallback. That is the maintenance win: a
//     model the CLI ships before AO's catalog learns about it is selectable
//     immediately.
func Merge(base []provider.ModelInfo, wire []claude.WireModel) ([]provider.ModelInfo, []Drift) {
	models := provider.CloneModels(base)
	index := make(map[string]int, len(models))
	for i, model := range models {
		index[model.Slug] = i
	}

	var drift []Drift
	for _, group := range groupWireRows(wire) {
		drift = append(drift, group.drift...)
		if group.disabled {
			drift = append(drift, Drift{
				Model:  group.slug,
				Kind:   DriftDisabled,
				Detail: "wire marks this model disabled; left untouched",
			})
			continue
		}
		if position, known := index[group.slug]; known {
			drift = append(drift, applyWireCapabilities(&models[position], group)...)
			continue
		}
		model, added := newWireOnlyModel(group, base)
		index[group.slug] = len(models)
		models = append(models, model)
		drift = append(drift, added...)
	}
	return models, drift
}

// wireGroup is every row that resolves to one canonical model, reduced to the
// single answer the merge uses.
type wireGroup struct {
	slug string
	// No display name is collected: a row's name names a ROW, and a
	// wire-only model's name is derived from its slug instead
	// (displayNameFromSlug). See newWireOnlyModel.
	//
	// capabilities is the first row for this model; later rows that disagree
	// produce DriftRowConflict rather than overwriting it.
	capabilities claude.WireModel
	extended     bool
	disabled     bool
	drift        []Drift
}

func groupWireRows(wire []claude.WireModel) []wireGroup {
	groups := make([]wireGroup, 0, len(wire))
	position := make(map[string]int, len(wire))

	for _, row := range wire {
		slug := row.CanonicalSlug()
		if slug == "" {
			continue
		}
		at, seen := position[slug]
		if !seen {
			position[slug] = len(groups)
			groups = append(groups, wireGroup{
				slug:         slug,
				capabilities: row,
				extended:     row.DeclaresExtendedContext(),
				disabled:     row.Disabled,
			})
			continue
		}

		group := &groups[at]
		group.extended = group.extended || row.DeclaresExtendedContext()
		group.disabled = group.disabled || row.Disabled
		if !sameCapabilities(group.capabilities, row) {
			group.drift = append(group.drift, Drift{
				Model: slug,
				Kind:  DriftRowConflict,
				Detail: fmt.Sprintf(
					"rows %q and %q disagree about capabilities; kept %q",
					group.capabilities.Value, row.Value, group.capabilities.Value,
				),
			})
		}
	}
	return groups
}

func sameCapabilities(a, b claude.WireModel) bool {
	return a.SupportsFastMode == b.SupportsFastMode &&
		a.SupportsEffort == b.SupportsEffort &&
		slices.Equal(knownEffortLevels(a), knownEffortLevels(b))
}

// applyWireCapabilities overwrites one catalog model's capability flags with
// the running binary's answer, reporting every change as drift.
func applyWireCapabilities(model *provider.ModelInfo, group wireGroup) []Drift {
	var drift []Drift

	// Three-state on purpose: the catalog never states auto-mode
	// support, so a wire row that says it is the first (and only)
	// source. An absent key leaves the model at "unknown" — consumers
	// restrict Auto only on an explicit false.
	if group.capabilities.SupportsAutoMode != nil {
		supports := *group.capabilities.SupportsAutoMode
		model.SupportsAutoMode = &supports
	}

	wantsFast := group.capabilities.SupportsFastMode
	hasFast := slices.Contains(model.Capabilities, provider.ModelCapabilityFastMode)
	if wantsFast != hasFast {
		drift = append(drift, Drift{
			Model:  model.Slug,
			Kind:   DriftCapability,
			Detail: fmt.Sprintf("catalog fast_mode=%t, wire says %t", hasFast, wantsFast),
		})
		if wantsFast {
			model.Capabilities = append(model.Capabilities, provider.ModelCapabilityFastMode)
		} else {
			model.Capabilities = slices.DeleteFunc(model.Capabilities, func(capability string) bool {
				return capability == provider.ModelCapabilityFastMode
			})
			if len(model.Capabilities) == 0 {
				model.Capabilities = nil
			}
		}
	}

	levels := knownEffortLevels(group.capabilities)
	if !group.capabilities.SupportsEffort || len(levels) == 0 {
		if len(model.ReasoningEfforts) > 0 {
			drift = append(drift, Drift{
				Model: model.Slug,
				Kind:  DriftEffort,
				Detail: fmt.Sprintf(
					"catalog declares efforts %s, wire reports no effort support",
					strings.Join(effortSlugs(model.ReasoningEfforts), "/"),
				),
			})
			model.ReasoningEfforts = nil
		}
		return drift
	}

	preferred := defaultEffortOf(model.ReasoningEfforts, provider.DefaultReasoningEffort)
	merged := effortOptions(levels, preferred)
	if !slices.Equal(effortSlugs(model.ReasoningEfforts), effortSlugs(merged)) {
		drift = append(drift, Drift{
			Model: model.Slug,
			Kind:  DriftEffort,
			Detail: fmt.Sprintf(
				"catalog declares efforts %s, wire declares %s",
				strings.Join(effortSlugs(model.ReasoningEfforts), "/"),
				strings.Join(effortSlugs(merged), "/"),
			),
		})
	}
	model.ReasoningEfforts = merged
	return drift
}

// newWireOnlyModel builds the catalog entry for a model the wire lists and the
// hand catalog does not know yet.
//
// Its name is DERIVED FROM THE SLUG, not taken from the row's displayName.
// A row name describes a picker row, and the CLI's row names are not unique
// per model: `claude-fable-5-1` arrives as "Fable", which sits in the picker
// next to the catalog's "Claude Fable 5" as a second, less specific entry for
// what looks like the same thing. The slug is the one thing that is per-model
// by construction.
//
// Context windows come from the closest catalog FAMILY (progressive
// trailing-segment trim, the same shape internal/usagecost uses to price an
// unseen model), because the wire reports no windows at all and a model with
// no windows is unselectable in the picker's context menu. With no family
// match the model gets standard-200k only — the tier every Claude model has —
// widened to 1M only when a wire row proves it by carrying the `[1m]` marker.
func newWireOnlyModel(group wireGroup, base []provider.ModelInfo) (provider.ModelInfo, []Drift) {
	model := provider.ModelInfo{
		Slug:     group.slug,
		Name:     displayNameFromSlug(group.slug),
		Provider: string(provider.Claude),
	}
	if group.capabilities.SupportsFastMode {
		model.Capabilities = []string{provider.ModelCapabilityFastMode}
	}
	if group.capabilities.SupportsAutoMode != nil {
		supports := *group.capabilities.SupportsAutoMode
		model.SupportsAutoMode = &supports
	}

	family, matched := familyMatch(group.slug, base)
	detail := "no catalog family matched; defaulted to standard context only"
	if matched {
		model.ContextWindows = slices.Clone(family.ContextWindows)
		detail = fmt.Sprintf("context windows inherited from catalog family %q", family.Slug)
	} else {
		model.ContextWindows = standardContextOptions()
	}
	if group.extended && !hasExtendedTier(model.ContextWindows) {
		// A wire row naming `<model>[1m]` is proof the 1M tier exists for it.
		// Added opt-in (never the default): the wire says the tier is
		// available, not that it should be paid for by default.
		model.ContextWindows = append(model.ContextWindows, provider.ContextWindowOption{
			Tokens: provider.ClaudeExtendedContextWindow,
			Label:  "1m",
			Tier:   provider.ContextTierExtended,
		})
		detail += "; 1M tier added from the wire's [1m] marker"
	}

	if levels := knownEffortLevels(group.capabilities); group.capabilities.SupportsEffort && len(levels) > 0 {
		preferred := provider.DefaultReasoningEffort
		if matched {
			preferred = defaultEffortOf(family.ReasoningEfforts, preferred)
		}
		model.ReasoningEfforts = effortOptions(levels, preferred)
	}

	return model, []Drift{{
		Model:  group.slug,
		Kind:   DriftFamilyDefault,
		Detail: detail,
	}}
}

// displayNameFromSlug renders a model slug as a picker label: hyphen
// segments become words, alphabetic ones are title-cased, and a RUN of
// numeric ones joins on "." — so `claude-fable-5-1` reads "Claude Fable 5.1"
// rather than "Claude Fable 5 1", and cannot be confused with the catalog's
// own "Claude Fable 5".
//
// Deriving the label instead of adopting the wire's `displayName` is the
// point: a row name ("Fable", "Opus (1M context)") names a picker row, and
// two models can share one.
func displayNameFromSlug(slug string) string {
	var name strings.Builder
	name.Grow(len(slug))
	afterNumber := false
	for _, segment := range strings.Split(slug, "-") {
		if segment == "" {
			continue
		}
		numeric := isNumericSegment(segment)
		switch {
		case name.Len() == 0:
		case numeric && afterNumber:
			name.WriteByte('.')
		default:
			name.WriteByte(' ')
		}
		if numeric {
			name.WriteString(segment)
		} else {
			name.WriteString(titleCaseSegment(segment))
		}
		afterNumber = numeric
	}
	if name.Len() == 0 {
		// A slug of nothing but separators. Nothing better to say than the
		// identifier itself.
		return slug
	}
	return name.String()
}

func isNumericSegment(segment string) bool {
	for i := 0; i < len(segment); i++ {
		if segment[i] < '0' || segment[i] > '9' {
			return false
		}
	}
	return true
}

// titleCaseSegment upper-cases the first rune and leaves the rest alone: the
// tail of a model segment is never prose, so "gpt"-style casing must not be
// invented or destroyed beyond the first letter.
func titleCaseSegment(segment string) string {
	runes := []rune(segment)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// familyMatch finds the catalog model whose slug shares the longest family
// prefix with slug, by trimming trailing "-"/"." segments off slug and
// prefix-matching the catalog. The prefix must keep at least two segments:
// with a one-segment floor every unknown model would match "claude-" and
// inherit the first catalog entry's windows, which is a guess dressed as data.
func familyMatch(slug string, base []provider.ModelInfo) (provider.ModelInfo, bool) {
	candidate := slug
	for {
		cut := strings.LastIndexAny(candidate, "-.")
		if cut <= 0 {
			return provider.ModelInfo{}, false
		}
		candidate = candidate[:cut]
		if segmentCount(candidate) < 2 {
			return provider.ModelInfo{}, false
		}
		for _, model := range base {
			if model.Slug == candidate ||
				strings.HasPrefix(model.Slug, candidate+"-") ||
				strings.HasPrefix(model.Slug, candidate+".") {
				return model, true
			}
		}
	}
}

func segmentCount(slug string) int {
	return 1 + strings.Count(slug, "-") + strings.Count(slug, ".")
}

func standardContextOptions() []provider.ContextWindowOption {
	return []provider.ContextWindowOption{{
		Tokens:  provider.ClaudeStandardContextWindow,
		Label:   "200k",
		Tier:    provider.ContextTierStandard,
		Default: true,
	}}
}

func hasExtendedTier(options []provider.ContextWindowOption) bool {
	return slices.ContainsFunc(options, func(option provider.ContextWindowOption) bool {
		return option.Tier == provider.ContextTierExtended
	})
}

// knownEffortLevels filters the wire's effort levels down to the enum AO can
// actually carry through a session, preserving wire order. An unknown level is
// dropped rather than passed on: SessionOptions would coerce it away later
// anyway, and a picker row that silently selects something else is worse than
// a missing row.
func knownEffortLevels(row claude.WireModel) []provider.ReasoningEffort {
	levels := make([]provider.ReasoningEffort, 0, len(row.SupportedEffortLevels))
	for _, level := range row.SupportedEffortLevels {
		effort := provider.ReasoningEffort(strings.TrimSpace(level))
		if effort == "" || !slices.Contains(provider.AllReasoningEfforts, effort) {
			continue
		}
		if slices.Contains(levels, effort) {
			continue
		}
		levels = append(levels, effort)
	}
	return levels
}

func effortOptions(levels []provider.ReasoningEffort, preferred provider.ReasoningEffort) []provider.ReasoningEffortOption {
	fallback := pickDefaultEffort(levels, preferred)
	options := make([]provider.ReasoningEffortOption, 0, len(levels))
	for _, level := range levels {
		options = append(options, provider.NewReasoningEffortOption(string(level), level == fallback))
	}
	return options
}

// pickDefaultEffort keeps preferred when the wire still offers it, and
// otherwise steps DOWN to the highest level below it (never up: the catalog's
// default is a deliberate cost/latency choice, and silently raising a model's
// effort because the wire dropped a tier would spend the user's tokens).
// Falls back to the lowest offered level when everything is above preferred.
func pickDefaultEffort(levels []provider.ReasoningEffort, preferred provider.ReasoningEffort) provider.ReasoningEffort {
	if len(levels) == 0 {
		return ""
	}
	if slices.Contains(levels, preferred) {
		return preferred
	}
	preferredRank := slices.Index(provider.AllReasoningEfforts, preferred)
	best := provider.ReasoningEffort("")
	bestRank := -1
	lowest := levels[0]
	lowestRank := slices.Index(provider.AllReasoningEfforts, lowest)
	for _, level := range levels {
		rank := slices.Index(provider.AllReasoningEfforts, level)
		if rank < lowestRank {
			lowest, lowestRank = level, rank
		}
		if preferredRank >= 0 && rank < preferredRank && rank > bestRank {
			best, bestRank = level, rank
		}
	}
	if bestRank >= 0 {
		return best
	}
	return lowest
}

func defaultEffortOf(options []provider.ReasoningEffortOption, fallback provider.ReasoningEffort) provider.ReasoningEffort {
	for _, option := range options {
		if option.Default {
			return provider.ReasoningEffort(option.Slug)
		}
	}
	return fallback
}

func effortSlugs(options []provider.ReasoningEffortOption) []string {
	slugs := make([]string, 0, len(options))
	for _, option := range options {
		slugs = append(slugs, option.Slug)
	}
	return slugs
}

// Catalog holds the merged picker catalog per probe identity.
//
// Keyed by provider.ProbeCacheKey because the answer depends on exactly what
// the probe's own answer depends on: which binary ran, whose credentials it
// held, from which directory, under which custom environment. Reading through
// the same key is what makes it impossible to serve one environment's model
// list to another.
//
// Deliberately NOT tied to the probe cache's TTL or its invalidations. A model
// list has no correctness deadline — dropping it when identity is rechecked
// would make wire-only models vanish from an open picker for the seconds a
// re-probe takes, and every probe replaces the entry wholesale anyway. The
// entry count is capped instead.
type Catalog struct {
	mu      sync.Mutex
	base    []provider.ModelInfo
	entries map[string]catalogEntry
	order   []string
}

type catalogEntry struct {
	models []provider.ModelInfo
	drift  string
}

// maxCatalogEntries bounds the map. Keys vary with binary, account, and custom
// environment; each entry costs a handful of models, and only a real probe
// (one subprocess, cached for minutes) can mint one, so a small cap is
// generous. The oldest write is evicted, which at worst costs the next lookup
// for that identity the enrichment until its next probe.
const maxCatalogEntries = 8

// NewCatalog returns a Catalog over the shipped Claude catalog.
func NewCatalog() *Catalog {
	return NewCatalogWith(provider.ClaudeModels)
}

// NewCatalogWith returns a Catalog over an explicit base list. Tests use it to
// exercise the merge against a small, stable catalog.
func NewCatalogWith(base []provider.ModelInfo) *Catalog {
	return &Catalog{
		base:    provider.CloneModels(base),
		entries: make(map[string]catalogEntry),
	}
}

// Store records the models one probe reported under its identity and returns
// the drift worth logging — nil when there is nothing to say, and nil when the
// same drift was already reported for this key, so a caller that logs on every
// probe result logs each distinct report once.
//
// wireErr is the decode outcome from claude.ProbeConfig.OnModels and is
// handled here rather than at the call site so no caller can get the rule
// wrong: an unreadable array is NO information, so the previous entry stands.
// An empty (or absent) array IS information — a binary that reports no models
// — so it replaces the entry with the plain catalog.
func (c *Catalog) Store(key provider.ProbeCacheKey, wire []claude.WireModel, wireErr error) []Drift {
	if wireErr != nil {
		return []Drift{{
			Kind:   DriftUnreadable,
			Detail: wireErr.Error(),
		}}
	}

	models, drift := Merge(c.base, wire)
	report := FormatDrift(drift)

	c.mu.Lock()
	defer c.mu.Unlock()

	encoded := key.String()
	previous, existed := c.entries[encoded]
	c.entries[encoded] = catalogEntry{models: models, drift: report}
	if !existed {
		c.order = append(c.order, encoded)
		c.evictOldestLocked()
	}
	if existed && previous.drift == report {
		return nil
	}
	return drift
}

func (c *Catalog) evictOldestLocked() {
	for len(c.order) > maxCatalogEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// ModelsFor returns the picker catalog for one probe identity, stamped for
// providerName. Falls back to the un-enriched catalog when no probe has
// reported yet — never to an empty list, and never to a spawn: this type only
// ever reads what a probe already handed it.
//
// providerName must be a provider whose ModelCatalog is
// provider.ClaudeProbeEnrichedCatalog (claude, claude-tui). Anything else
// returns nil, so a miswired caller shows an empty picker rather than Claude's
// models under another provider's name.
func (c *Catalog) ModelsFor(key provider.ProbeCacheKey, providerName string) []provider.ModelInfo {
	if provider.CapabilitiesForProvider(providerName).ModelCatalog != provider.ClaudeProbeEnrichedCatalog {
		return nil
	}

	c.mu.Lock()
	source := c.base
	if entry, ok := c.entries[key.String()]; ok {
		source = entry.models
	}
	models := provider.CloneModels(source)
	c.mu.Unlock()

	for i := range models {
		models[i].Provider = providerName
	}
	return models
}
