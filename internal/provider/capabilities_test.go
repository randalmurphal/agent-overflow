package provider

import (
	"slices"
	"testing"
)

// TestProviderRegistryExhaustiveness pins that every ProviderKind in
// AllProviders() is explicitly wired into capabilities, usage family,
// context windows, and reasoning-effort sets.
func TestProviderRegistryExhaustiveness(t *testing.T) {
	t.Parallel()
	if len(AllProviders()) == 0 {
		t.Fatal("AllProviders() is empty")
	}
	if len(StandaloneProviders()) == 0 {
		t.Fatal("StandaloneProviders() is empty")
	}

	seen := make(map[ProviderKind]bool, len(AllProviders()))
	for _, kind := range AllProviders() {
		if kind == "" {
			t.Fatal("AllProviders() contains empty ProviderKind")
		}
		if seen[kind] {
			t.Fatalf("AllProviders() repeats %q", kind)
		}
		seen[kind] = true

		name := string(kind)
		if !IsKnownProvider(name) {
			t.Errorf("IsKnownProvider(%q) = false", name)
		}
		caps := CapabilitiesForProvider(name)
		if caps.UsageFamily == "" {
			t.Errorf("CapabilitiesForProvider(%q).UsageFamily is empty", name)
		}
		if !slices.Contains(StandaloneProviders(), caps.UsageFamily) {
			t.Errorf("CapabilitiesForProvider(%q).UsageFamily = %q, want a standalone provider", name, caps.UsageFamily)
		}
		if caps.StandardContextWindow <= 0 {
			t.Errorf("CapabilitiesForProvider(%q).StandardContextWindow = %d, want > 0", name, caps.StandardContextWindow)
		}
		if caps.ExtendedContextWindow <= 0 {
			t.Errorf("CapabilitiesForProvider(%q).ExtendedContextWindow = %d, want > 0", name, caps.ExtendedContextWindow)
		}
		if len(ReasoningEffortsForProvider(name)) == 0 {
			t.Errorf("ReasoningEffortsForProvider(%q) is empty", name)
		}
	}

	for _, kind := range StandaloneProviders() {
		if !seen[kind] {
			t.Errorf("StandaloneProviders() contains %q which is absent from AllProviders()", kind)
		}
	}

	if slices.Contains(StandaloneProviders(), ClaudeTUI) {
		t.Error("StandaloneProviders() lists dependent provider claude-tui")
	}
	if IsKnownProvider("unknown-provider") {
		t.Error("IsKnownProvider accepted an unknown provider")
	}
}
