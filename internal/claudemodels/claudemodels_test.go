package claudemodels

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// capturedWireModels is the real 2.1.219 model list, read out of the same
// trimmed `initialize` capture the parser test uses. Merge policy is decided
// against what the CLI actually says, not against a hand-written idea of it.
func capturedWireModels(t *testing.T) []claude.WireModel {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(
		"../../docs/references/fixtures/claude/initialize_models_20260802.json",
	))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var envelope struct {
		Response struct {
			Response struct {
				Models []claude.WireModel `json:"models"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(envelope.Response.Response.Models) == 0 {
		t.Fatal("fixture carries no models")
	}
	return envelope.Response.Response.Models
}

func findModel(models []provider.ModelInfo, slug string) (provider.ModelInfo, bool) {
	for _, model := range models {
		if model.Slug == slug {
			return model, true
		}
	}
	return provider.ModelInfo{}, false
}

func slugs(models []provider.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.Slug)
	}
	return out
}

func driftKinds(drift []Drift, model string) []DriftKind {
	var kinds []DriftKind
	for _, d := range drift {
		if d.Model == model {
			kinds = append(kinds, d.Kind)
		}
	}
	return kinds
}

func effortSlugsOf(options []provider.ReasoningEffortOption) []string {
	return effortSlugs(options)
}

// TestMergeAgainstTheShippedCatalog is the end-to-end policy test over the
// real capture and the real catalog: nothing disappears, nothing is reordered,
// aliases collapse, and the two wire rows for Opus do not produce two Opuses.
func TestMergeAgainstTheShippedCatalog(t *testing.T) {
	before := slugs(provider.ClaudeModels)
	merged, drift := Merge(provider.ClaudeModels, capturedWireModels(t))

	after := slugs(merged)
	if len(after) < len(before) || !slices.Equal(after[:len(before)], before) {
		t.Fatalf("catalog models must survive in order: got %v, want prefix %v", after, before)
	}
	for _, slug := range []string{"claude-opus-4-8", "claude-opus-4-5", "claude-sonnet-4-6"} {
		if _, ok := findModel(merged, slug); !ok {
			t.Errorf("%s is absent from the wire but still usable — it must stay listed", slug)
		}
	}
	if count := slices.Index(after, "claude-opus-5"); count < 0 {
		t.Fatal("claude-opus-5 missing")
	}
	occurrences := 0
	for _, slug := range after {
		if slug == "claude-opus-5" {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Errorf("the `default` and `opus[1m]` rows must collapse onto one model, got %d entries", occurrences)
	}

	// Today's capture agrees with the catalog everywhere (Haiku's effort list
	// was corrected from it), so a clean merge is the expected state. A drift
	// line here means the shipped catalog needs updating.
	if len(drift) != 0 {
		t.Errorf("shipped catalog disagrees with the captured wire: %s", FormatDrift(drift))
	}
}

func TestMergeKeepsCatalogContextWindowsAndNames(t *testing.T) {
	merged, _ := Merge(provider.ClaudeModels, capturedWireModels(t))

	opus, ok := findModel(merged, "claude-opus-5")
	if !ok {
		t.Fatal("claude-opus-5 missing")
	}
	// "Opus (1M context)" / "Default (recommended)" name picker ROWS, not the
	// model — the catalog keeps naming rights.
	if opus.Name != "Claude Opus 5" {
		t.Errorf("Name = %q, want the catalog's own name", opus.Name)
	}
	if len(opus.ContextWindows) != 2 {
		t.Fatalf("ContextWindows = %v, want the catalog's 200k/1M pair", opus.ContextWindows)
	}
	if tokens, ok := provider.DefaultContextWindowForOptions(opus.ContextWindows); !ok ||
		tokens != provider.ClaudeExtendedContextWindow {
		t.Errorf("default context window = %d, want the catalog's 1M default", tokens)
	}
}

// TestMergeAddsWireOnlyModelWithFamilyWindows is the maintenance win: a model
// the CLI ships before AO's catalog knows it is selectable immediately, with
// the context windows of its closest family.
func TestMergeAddsWireOnlyModelWithFamilyWindows(t *testing.T) {
	wire := []claude.WireModel{{
		Value:                 "opus",
		ResolvedModel:         "claude-opus-6[1m]",
		DisplayName:           "Opus 6",
		SupportsEffort:        true,
		SupportedEffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
		SupportsFastMode:      true,
	}}

	merged, drift := Merge(provider.ClaudeModels, wire)

	model, ok := findModel(merged, "claude-opus-6")
	if !ok {
		t.Fatalf("wire-only model missing from the picker catalog: %v", slugs(merged))
	}
	if merged[len(merged)-1].Slug != "claude-opus-6" {
		t.Error("wire-only models append; they must not displace the catalog's first entry")
	}
	if model.Name != "Opus 6" {
		t.Errorf("Name = %q, want the wire display name (we have nothing better)", model.Name)
	}
	if !slices.Contains(model.Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("fast-mode capability must come across from the wire")
	}
	family, _ := findModel(provider.ClaudeModels, "claude-opus-5")
	if !slices.Equal(model.ContextWindows, family.ContextWindows) {
		t.Errorf("ContextWindows = %v, want the claude-opus family's %v", model.ContextWindows, family.ContextWindows)
	}
	if got := effortSlugsOf(model.ReasoningEfforts); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("ReasoningEfforts = %v, want the wire's levels", got)
	}
	if kinds := driftKinds(drift, "claude-opus-6"); !slices.Contains(kinds, DriftFamilyDefault) {
		t.Errorf("a family-defaulted model must be reported: %v", FormatDrift(drift))
	}
}

func TestMergeWireOnlyModelWithNoFamilyGetsStandardContextOnly(t *testing.T) {
	wire := []claude.WireModel{{
		Value:       "claude-newthing-1",
		DisplayName: "Newthing",
	}}

	merged, drift := Merge(provider.ClaudeModels, wire)

	model, ok := findModel(merged, "claude-newthing-1")
	if !ok {
		t.Fatal("wire-only model missing")
	}
	if len(model.ContextWindows) != 1 ||
		model.ContextWindows[0].Tokens != provider.ClaudeStandardContextWindow ||
		!model.ContextWindows[0].Default {
		t.Errorf("ContextWindows = %v, want standard-200k-only", model.ContextWindows)
	}
	if len(model.ReasoningEfforts) != 0 {
		t.Errorf("ReasoningEfforts = %v, want none (the row declares none)", model.ReasoningEfforts)
	}
	if kinds := driftKinds(drift, "claude-newthing-1"); !slices.Contains(kinds, DriftFamilyDefault) {
		t.Errorf("drift = %s, want a family-default report", FormatDrift(drift))
	}
}

// TestMergeWireOnlyModelTakesExtendedTierFromTheMarker: with no family to
// inherit from, the `[1m]` marker is the only evidence the 1M tier exists —
// and it is positive evidence, so it widens the fallback (opt-in, never the
// default).
func TestMergeWireOnlyModelTakesExtendedTierFromTheMarker(t *testing.T) {
	wire := []claude.WireModel{{
		Value:       "claude-newthing-1[1m]",
		DisplayName: "Newthing",
	}}

	merged, _ := Merge(provider.ClaudeModels, wire)

	model, ok := findModel(merged, "claude-newthing-1")
	if !ok {
		t.Fatal("wire-only model missing")
	}
	if len(model.ContextWindows) != 2 {
		t.Fatalf("ContextWindows = %v, want the standard tier widened by 1M", model.ContextWindows)
	}
	extended := model.ContextWindows[1]
	if extended.Tokens != provider.ClaudeExtendedContextWindow || extended.Tier != provider.ContextTierExtended {
		t.Errorf("second tier = %+v, want the 1M extended tier", extended)
	}
	if extended.Default {
		t.Error("the 1M tier must be opt-in: the wire proves it exists, not that it should be paid for")
	}
	if tokens, ok := provider.DefaultContextWindowForOptions(model.ContextWindows); !ok ||
		tokens != provider.ClaudeStandardContextWindow {
		t.Errorf("default = %d, want the standard tier", tokens)
	}
}

// TestMergeAppliesWireCapabilityOverrides covers the brief's decided policy:
// for a model the wire lists, the running binary's capability flags win over
// a stale catalog — in both directions — and every change is reported.
func TestMergeAppliesWireCapabilityOverrides(t *testing.T) {
	base := []provider.ModelInfo{
		{
			Slug:             "claude-sonnet-5",
			Name:             "Claude Sonnet 5",
			Provider:         "claude",
			Capabilities:     []string{provider.ModelCapabilityFastMode},
			ContextWindows:   []provider.ContextWindowOption{{Tokens: 200000, Label: "200k", Tier: provider.ContextTierStandard, Default: true}},
			ReasoningEfforts: []provider.ReasoningEffortOption{{Slug: "low"}, {Slug: "high", Default: true}},
		},
		{
			Slug:           "claude-haiku-4-5",
			Name:           "Claude Haiku 4.5",
			Provider:       "claude",
			ContextWindows: []provider.ContextWindowOption{{Tokens: 200000, Label: "200k", Tier: provider.ContextTierStandard, Default: true}},
			ReasoningEfforts: []provider.ReasoningEffortOption{
				{Slug: "low"}, {Slug: "medium"}, {Slug: "high", Default: true},
			},
		},
	}
	wire := []claude.WireModel{
		{
			Value:                 "sonnet",
			ResolvedModel:         "claude-sonnet-5",
			DisplayName:           "Sonnet",
			SupportsEffort:        true,
			SupportedEffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
			// Wire says no fast mode; the catalog claims it.
		},
		{
			Value:         "haiku",
			ResolvedModel: "claude-haiku-4-5-20251001",
			DisplayName:   "Haiku",
			// No effort support at all — the spike's real discrepancy.
		},
	}

	merged, drift := Merge(base, wire)

	sonnet, _ := findModel(merged, "claude-sonnet-5")
	if slices.Contains(sonnet.Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("wire says Sonnet has no fast mode; the wire wins")
	}
	if got := effortSlugsOf(sonnet.ReasoningEfforts); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("effort levels = %v, want the wire's", got)
	}
	if got := defaultEffortOf(sonnet.ReasoningEfforts, ""); got != provider.EffortHigh {
		t.Errorf("default effort = %q, want the catalog's own default preserved", got)
	}
	if kinds := driftKinds(drift, "claude-sonnet-5"); !slices.Contains(kinds, DriftCapability) ||
		!slices.Contains(kinds, DriftEffort) {
		t.Errorf("both overrides must be reported, got %v", kinds)
	}

	haiku, _ := findModel(merged, "claude-haiku-4-5")
	if len(haiku.ReasoningEfforts) != 0 {
		t.Errorf("ReasoningEfforts = %v, want none — the wire reports no effort support", haiku.ReasoningEfforts)
	}
	if kinds := driftKinds(drift, "claude-haiku-4-5"); !slices.Contains(kinds, DriftEffort) {
		t.Errorf("the effort discrepancy must be reported, got %v", kinds)
	}
}

func TestMergeAddsFastModeCapabilityTheCatalogLacks(t *testing.T) {
	base := []provider.ModelInfo{{
		Slug:     "claude-sonnet-5",
		Name:     "Claude Sonnet 5",
		Provider: "claude",
	}}
	wire := []claude.WireModel{{
		Value:            "sonnet",
		ResolvedModel:    "claude-sonnet-5",
		SupportsFastMode: true,
	}}

	merged, drift := Merge(base, wire)

	if !slices.Contains(merged[0].Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("the wire granting a capability must be applied, not only a wire revoking one")
	}
	if kinds := driftKinds(drift, "claude-sonnet-5"); !slices.Contains(kinds, DriftCapability) {
		t.Errorf("kinds = %v, want a capability drift line", kinds)
	}
}

// TestMergeNeverRaisesTheDefaultEffort: when the wire drops the tier the
// catalog defaulted to, the merge steps DOWN. Silently promoting a model to a
// costlier tier because a wire list changed is the one failure mode that
// spends the user's money.
func TestMergeNeverRaisesTheDefaultEffort(t *testing.T) {
	base := []provider.ModelInfo{{
		Slug:     "claude-opus-5",
		Name:     "Claude Opus 5",
		Provider: "claude",
		ReasoningEfforts: []provider.ReasoningEffortOption{
			{Slug: "low"}, {Slug: "high"}, {Slug: "xhigh", Default: true},
		},
	}}
	wire := []claude.WireModel{{
		Value:                 "opus",
		ResolvedModel:         "claude-opus-5",
		SupportsEffort:        true,
		SupportedEffortLevels: []string{"low", "high", "max"},
	}}

	merged, _ := Merge(base, wire)

	if got := defaultEffortOf(merged[0].ReasoningEfforts, ""); got != provider.EffortHigh {
		t.Errorf("default effort = %q, want high (the highest tier below the catalog's xhigh)", got)
	}
}

func TestMergeDropsEffortLevelsAOCannotCarry(t *testing.T) {
	base := []provider.ModelInfo{{Slug: "claude-sonnet-5", Name: "Sonnet", Provider: "claude"}}
	wire := []claude.WireModel{{
		Value:                 "sonnet",
		ResolvedModel:         "claude-sonnet-5",
		SupportsEffort:        true,
		SupportedEffortLevels: []string{"low", "galactic", "high", "low"},
	}}

	merged, _ := Merge(base, wire)

	if got := effortSlugsOf(merged[0].ReasoningEfforts); !slices.Equal(got, []string{"low", "high"}) {
		t.Errorf("effort levels = %v, want the known levels, de-duplicated", got)
	}
}

// TestMergeReportsConflictingRowsAndKeepsTheFirst: two rows for one model that
// disagree cannot both be right, and picking silently would make the answer
// depend on wire order.
func TestMergeReportsConflictingRowsAndKeepsTheFirst(t *testing.T) {
	base := []provider.ModelInfo{{Slug: "claude-opus-5", Name: "Opus", Provider: "claude"}}
	wire := []claude.WireModel{
		{Value: "opus[1m]", ResolvedModel: "claude-opus-5[1m]", SupportsFastMode: true},
		{Value: "default", ResolvedModel: "claude-opus-5[1m]"},
	}

	merged, drift := Merge(base, wire)

	if !slices.Contains(merged[0].Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("the first row's answer must stand")
	}
	if kinds := driftKinds(drift, "claude-opus-5"); !slices.Contains(kinds, DriftRowConflict) {
		t.Errorf("kinds = %v, want a row-conflict report", kinds)
	}
}

// TestMergeReportsDisabledRowsWithoutActingOnThem: `disabled` is in the CLI's
// schema but has never appeared in a capture, so it is reported and nothing
// else — hiding a model on an unverified field is the worse failure.
func TestMergeReportsDisabledRowsWithoutActingOnThem(t *testing.T) {
	base := []provider.ModelInfo{{
		Slug:         "claude-opus-5",
		Name:         "Opus",
		Provider:     "claude",
		Capabilities: []string{provider.ModelCapabilityFastMode},
	}}
	wire := []claude.WireModel{
		{Value: "opus", ResolvedModel: "claude-opus-5", Disabled: true},
		{Value: "claude-newthing-1", DisplayName: "Newthing", Disabled: true},
	}

	merged, drift := Merge(base, wire)

	if len(merged) != 1 {
		t.Errorf("a disabled wire-only model must not be added: %v", slugs(merged))
	}
	if !slices.Contains(merged[0].Capabilities, provider.ModelCapabilityFastMode) {
		t.Error("a disabled row must not strip a catalog model's capabilities")
	}
	if kinds := driftKinds(drift, "claude-opus-5"); !slices.Contains(kinds, DriftDisabled) {
		t.Errorf("kinds = %v, want a disabled report", kinds)
	}
}

func TestMergeIgnoresRowsThatNameNothing(t *testing.T) {
	base := []provider.ModelInfo{{Slug: "claude-opus-5", Name: "Opus", Provider: "claude"}}
	merged, drift := Merge(base, []claude.WireModel{{DisplayName: "Nameless"}})
	if len(merged) != 1 {
		t.Errorf("merged = %v, want the catalog untouched", slugs(merged))
	}
	if len(drift) != 0 {
		t.Errorf("drift = %s, want nothing to report", FormatDrift(drift))
	}
}

func TestMergeDoesNotMutateTheBaseCatalog(t *testing.T) {
	base := []provider.ModelInfo{{
		Slug:             "claude-sonnet-5",
		Name:             "Claude Sonnet 5",
		Provider:         "claude",
		Capabilities:     []string{provider.ModelCapabilityFastMode},
		ReasoningEfforts: []provider.ReasoningEffortOption{{Slug: "high", Default: true}},
	}}
	wire := []claude.WireModel{{Value: "sonnet", ResolvedModel: "claude-sonnet-5"}}

	if _, _ = Merge(base, wire); len(base[0].Capabilities) != 1 || len(base[0].ReasoningEfforts) != 1 {
		t.Fatalf("Merge mutated its input: %+v", base[0])
	}
}

// --- Catalog ---

func testKey(account string) provider.ProbeCacheKey {
	return provider.ProbeCacheKey{Binary: "/usr/bin/claude", AccountID: account, WorkDir: "/home/u"}
}

func TestCatalogFallsBackToTheShippedListBeforeAnyProbe(t *testing.T) {
	catalog := NewCatalog()
	models := catalog.ModelsFor(testKey("a"), string(provider.Claude))
	if !slices.Equal(slugs(models), slugs(provider.ClaudeModels)) {
		t.Errorf("models = %v, want the shipped catalog", slugs(models))
	}
}

func TestCatalogStampsTheRequestedProvider(t *testing.T) {
	catalog := NewCatalog()
	for _, name := range []string{string(provider.Claude), string(provider.ClaudeTUI)} {
		for _, model := range catalog.ModelsFor(testKey("a"), name) {
			if model.Provider != name {
				t.Fatalf("ModelsFor(%q) returned a %q model", name, model.Provider)
			}
		}
	}
	if models := catalog.ModelsFor(testKey("a"), string(provider.Codex)); models != nil {
		t.Errorf("ModelsFor(codex) = %v, want nil — Claude models must not be served under another provider", slugs(models))
	}
}

func TestCatalogServesOnlyTheMatchingIdentity(t *testing.T) {
	catalog := NewCatalog()
	catalog.Store(testKey("account-a"), []claude.WireModel{{
		Value: "claude-newthing-1", DisplayName: "Newthing",
	}}, nil)

	if _, ok := findModel(catalog.ModelsFor(testKey("account-a"), string(provider.Claude)), "claude-newthing-1"); !ok {
		t.Error("the storing identity must see its own enrichment")
	}
	if _, ok := findModel(catalog.ModelsFor(testKey("account-b"), string(provider.Claude)), "claude-newthing-1"); ok {
		t.Error("another identity must not be served this account's model list")
	}
}

// TestCatalogDriftIsReportedOncePerDistinctReport backs "a log line, once per
// probe result, never a toast": a probe that repeats the same story says
// nothing the second time, and a probe that tells a NEW story is heard.
func TestCatalogDriftIsReportedOncePerDistinctReport(t *testing.T) {
	catalog := NewCatalogWith([]provider.ModelInfo{{
		Slug:         "claude-opus-5",
		Name:         "Opus",
		Provider:     "claude",
		Capabilities: []string{provider.ModelCapabilityFastMode},
	}})
	wire := []claude.WireModel{{Value: "opus", ResolvedModel: "claude-opus-5"}}

	first := catalog.Store(testKey("a"), wire, nil)
	if len(first) == 0 {
		t.Fatal("the first probe must report the drift")
	}
	if second := catalog.Store(testKey("a"), wire, nil); len(second) != 0 {
		t.Errorf("an unchanged report must not repeat: %s", FormatDrift(second))
	}

	changed := []claude.WireModel{{Value: "claude-newthing-1", DisplayName: "Newthing"}}
	if third := catalog.Store(testKey("a"), changed, nil); len(third) == 0 {
		t.Error("a changed report must be heard again")
	}
}

// TestCatalogTransitionsBetweenReportingAndSilentBinaries is the state-machine
// half: a CLI that stops reporting models must not leave the previous list
// enriching a binary that no longer claims it, while an unreadable array —
// which is no information — must leave the last good answer alone.
func TestCatalogTransitionsBetweenReportingAndSilentBinaries(t *testing.T) {
	catalog := NewCatalog()
	key := testKey("a")
	wire := []claude.WireModel{{Value: "claude-newthing-1", DisplayName: "Newthing"}}

	catalog.Store(key, wire, nil)
	if _, ok := findModel(catalog.ModelsFor(key, string(provider.Claude)), "claude-newthing-1"); !ok {
		t.Fatal("enrichment did not land")
	}

	drift := catalog.Store(key, nil, errors.New("models: unreadable"))
	if _, ok := findModel(catalog.ModelsFor(key, string(provider.Claude)), "claude-newthing-1"); !ok {
		t.Error("an unreadable array is no information: the previous answer must stand")
	}
	if len(drift) != 1 || drift[0].Kind != DriftUnreadable {
		t.Errorf("drift = %v, want one unreadable report", drift)
	}

	catalog.Store(key, nil, nil)
	if _, ok := findModel(catalog.ModelsFor(key, string(provider.Claude)), "claude-newthing-1"); ok {
		t.Error("a binary that reports no models must clear the enrichment it previously supplied")
	}
	if !slices.Equal(slugs(catalog.ModelsFor(key, string(provider.Claude))), slugs(provider.ClaudeModels)) {
		t.Error("clearing the enrichment must leave the shipped catalog, never an empty picker")
	}
}

func TestCatalogBoundsItsEntries(t *testing.T) {
	catalog := NewCatalog()
	wire := []claude.WireModel{{Value: "claude-newthing-1", DisplayName: "Newthing"}}
	for i := 0; i < maxCatalogEntries+3; i++ {
		catalog.Store(provider.ProbeCacheKey{Binary: "claude", AccountID: string(rune('a' + i))}, wire, nil)
	}

	catalog.mu.Lock()
	entries, order := len(catalog.entries), len(catalog.order)
	catalog.mu.Unlock()
	if entries > maxCatalogEntries || order > maxCatalogEntries {
		t.Errorf("entries=%d order=%d, want both capped at %d", entries, order, maxCatalogEntries)
	}
}

func TestCatalogReturnsIndependentCopies(t *testing.T) {
	catalog := NewCatalog()
	key := testKey("a")
	catalog.Store(key, capturedWireModels(t), nil)

	first := catalog.ModelsFor(key, string(provider.Claude))
	first[0].Slug = "mutated"
	first[0].ContextWindows[0].Tokens = 1

	second := catalog.ModelsFor(key, string(provider.Claude))
	if second[0].Slug == "mutated" || second[0].ContextWindows[0].Tokens == 1 {
		t.Error("callers mutate what they get; the cached list must not follow")
	}
}

func TestFormatDriftIsEmptyWhenThereIsNothingToSay(t *testing.T) {
	if got := FormatDrift(nil); got != "" {
		t.Errorf("FormatDrift(nil) = %q, want empty", got)
	}
	got := FormatDrift([]Drift{
		{Model: "m", Kind: DriftEffort, Detail: "a"},
		{Kind: DriftUnreadable, Detail: "b"},
	})
	if got != "m [effort]: a; unreadable: b" {
		t.Errorf("FormatDrift = %q", got)
	}
}

// --- helpers ---

func TestFamilyMatchNeedsTwoSegments(t *testing.T) {
	base := []provider.ModelInfo{
		{Slug: "claude-fable-5"},
		{Slug: "claude-opus-5"},
		{Slug: "claude-opus-4-8"},
	}
	tests := []struct {
		slug string
		want string
	}{
		{slug: "claude-opus-6", want: "claude-opus-5"},
		{slug: "claude-opus-5-20260101", want: "claude-opus-5"},
		{slug: "claude-fable-6-mini", want: "claude-fable-5"},
		// One segment ("claude") would match the whole catalog — that is a
		// guess, not a family.
		{slug: "claude-newthing-1", want: ""},
		{slug: "sonnet", want: ""},
	}
	for _, tt := range tests {
		got, ok := familyMatch(tt.slug, base)
		if tt.want == "" {
			if ok {
				t.Errorf("familyMatch(%q) = %q, want no match", tt.slug, got.Slug)
			}
			continue
		}
		if !ok || got.Slug != tt.want {
			t.Errorf("familyMatch(%q) = %q/%v, want %q", tt.slug, got.Slug, ok, tt.want)
		}
	}
}

func TestPickDefaultEffort(t *testing.T) {
	levels := func(values ...string) []provider.ReasoningEffort {
		out := make([]provider.ReasoningEffort, 0, len(values))
		for _, value := range values {
			out = append(out, provider.ReasoningEffort(value))
		}
		return out
	}
	tests := []struct {
		name      string
		levels    []provider.ReasoningEffort
		preferred provider.ReasoningEffort
		want      provider.ReasoningEffort
	}{
		{name: "exact", levels: levels("low", "high"), preferred: provider.EffortHigh, want: provider.EffortHigh},
		{name: "steps down", levels: levels("low", "high", "max"), preferred: provider.EffortXHigh, want: provider.EffortHigh},
		{name: "lowest when all above", levels: levels("max", "xhigh"), preferred: provider.EffortLow, want: provider.EffortXHigh},
		{name: "empty", levels: nil, preferred: provider.EffortHigh, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickDefaultEffort(tt.levels, tt.preferred); got != tt.want {
				t.Errorf("pickDefaultEffort = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFixtureIsTheRealEnvelope guards the fixture itself: if someone
// regenerates it from a different capture shape, the parser tests and these
// merge tests must both notice.
func TestFixtureIsTheRealEnvelope(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(
		"../../docs/references/fixtures/claude/initialize_models_20260802.json",
	))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if !bytes.Contains(compact.Bytes(), []byte(`"type":"control_response"`)) {
		t.Error("fixture must stay a captured control_response envelope")
	}
}
