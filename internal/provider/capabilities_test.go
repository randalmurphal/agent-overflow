package provider

import "testing"

func TestCapabilitiesForProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     Capabilities
	}{
		{
			name:     "claude",
			provider: string(Claude),
			want: Capabilities{
				ModelCatalog:        ClaudeProbeEnrichedCatalog,
				EnforcesRuntimeMode: true,
			},
		},
		{
			name:     "claude-tui",
			provider: string(ClaudeTUI),
			want: Capabilities{
				ModelCatalog:   ClaudeProbeEnrichedCatalog,
				ImageIngestion: PathImageIngestion,
			},
		},
		{
			name:     "codex",
			provider: string(Codex),
			want: Capabilities{
				ModelCatalog:              CodexLiveModelCatalog,
				BackgroundTerminalCleaner: CodexBackgroundTerminalCleaner,
				ImageIngestion:            PathImageIngestion,
				EnforcesRuntimeMode:       true,
			},
		},
		{
			name:     "unknown",
			provider: "other",
			want:     Capabilities{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CapabilitiesForProvider(tt.provider); got != tt.want {
				t.Fatalf("CapabilitiesForProvider(%q) = %+v, want %+v", tt.provider, got, tt.want)
			}
		})
	}
}

// TestEnforcesRuntimeModeMatchesSessionConfig pins the capability to the
// actual behaviour of each provider's ConfigFromOptions rather than to a
// hand-maintained opinion. claude-tui deliberately drops the permission
// fields (approvals live inside the real TUI), so its threads' runtime mode
// is inert — callers that treat a runtime mode as a guarantee must refuse it.
func TestEnforcesRuntimeModeMatchesSessionConfig(t *testing.T) {
	enforcing := map[string]bool{
		string(Claude):    true,
		string(Codex):     true,
		string(ClaudeTUI): false,
		"other":           false,
	}
	for name, want := range enforcing {
		if got := CapabilitiesForProvider(name).EnforcesRuntimeMode; got != want {
			t.Errorf("CapabilitiesForProvider(%q).EnforcesRuntimeMode = %v, want %v", name, got, want)
		}
	}
}
