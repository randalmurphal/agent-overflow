package provider

import "testing"

// TestProviderRegistryExhaustiveness pins that every ProviderKind in
// AllProviders() is explicitly wired into capabilities, display naming,
// usage family, context windows, and reasoning-effort sets.
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
		if ProviderDisplayName(name) == "" || ProviderDisplayName(name) == name {
			t.Errorf("ProviderDisplayName(%q) = %q, want a human label", name, ProviderDisplayName(name))
		}
		caps := CapabilitiesForProvider(name)
		if caps.UsageFamily == "" {
			t.Errorf("CapabilitiesForProvider(%q).UsageFamily is empty", name)
		}
		if !IsStandaloneProvider(string(caps.UsageFamily)) {
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
		if !IsStandaloneProvider(string(kind)) {
			t.Errorf("IsStandaloneProvider(%q) = false", kind)
		}
	}

	if IsKnownProvider("unknown-provider") {
		t.Error("IsKnownProvider accepted an unknown provider")
	}
	if IsStandaloneProvider(string(ClaudeTUI)) {
		t.Error("IsStandaloneProvider accepted dependent provider claude-tui")
	}
}
