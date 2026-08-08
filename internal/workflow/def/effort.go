package def

import (
	"fmt"
	"strings"
)

// EffortTier names one reasoning tier an agent turn may be pinned to with
// `effort:`. The vocabulary is CLOSED: an unrecognised tier is a validation
// finding rather than a value silently coerced at run time, because a typo would
// otherwise read as "this phase deliberately runs at the model's default".
//
// The list is declared here rather than imported from `internal/provider`
// because this package is pure (see AGENTS.md §Boundaries) — a workflow
// definition is authored, validated, and published without any provider
// process in reach. The two vocabularies are held together by
// TestWorkflowEffortTiersMatchTheProviderReasoningEfforts in the root package,
// which fails if either side gains or loses a tier.
type EffortTier string

const (
	EffortNone    EffortTier = "none"
	EffortMinimal EffortTier = "minimal"
	EffortLow     EffortTier = "low"
	EffortMedium  EffortTier = "medium"
	EffortHigh    EffortTier = "high"
	EffortXHigh   EffortTier = "xhigh"
	EffortMax     EffortTier = "max"
	EffortUltra   EffortTier = "ultra"
)

// effortTiers is the vocabulary in ascending order. Order is part of the
// contract: it is what the published schema's enum and every diagnostic list,
// and it matches provider.AllReasoningEfforts so the drift test can compare the
// two as sequences rather than as sets.
var effortTiers = []EffortTier{
	EffortNone,
	EffortMinimal,
	EffortLow,
	EffortMedium,
	EffortHigh,
	EffortXHigh,
	EffortMax,
	EffortUltra,
}

// EffortTiers returns an isolated copy of the closed tier vocabulary, in
// ascending order.
func EffortTiers() []EffortTier { return append([]EffortTier(nil), effortTiers...) }

// EffortTierNames returns the tier vocabulary as strings, for diagnostics and
// the CLI's help output.
func EffortTierNames() []string {
	names := make([]string, 0, len(effortTiers))
	for _, tier := range effortTiers {
		names = append(names, string(tier))
	}
	return names
}

// KnownEffortTier reports whether name is one of the closed tiers.
func KnownEffortTier(name string) bool {
	for _, tier := range effortTiers {
		if string(tier) == name {
			return true
		}
	}
	return false
}

// effortTierFindings checks the tier VALUE of one declared `effort:`. Whether
// the element may declare one at all is enforced where the other agent-turn
// fields are refused (validatePhaseExecution, validateCall,
// fanOutPhaseFieldFindings, validateUnitDefinition), so this answers only "is
// this a tier we recognise".
//
// Per-model legality is deliberately NOT checked here. Which tiers a given
// model advertises is provider-owned and partly live catalog data (Codex's
// model list comes off the app-server, Claude's is probe-enriched), so a
// definition that validates today would fail tomorrow for reasons the author
// cannot see in the YAML. An authored tier the model does not advertise is
// coerced onto the model's own default at thread creation instead
// (`createWorkflowThread`, repo root).
func effortTierFindings(effort, element string) []Finding {
	trimmed := strings.TrimSpace(effort)
	if trimmed == "" || KnownEffortTier(trimmed) {
		return nil
	}
	return []Finding{finding("phase.effort", element, fmt.Sprintf(
		"unknown effort %q; available tiers are %s",
		effort, strings.Join(EffortTierNames(), ", "),
	))}
}
