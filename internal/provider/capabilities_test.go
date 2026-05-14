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
			want:     Capabilities{},
		},
		{
			name:     "codex",
			provider: string(Codex),
			want: Capabilities{
				ModelCatalog:              CodexLiveModelCatalog,
				BackgroundTerminalCleaner: CodexBackgroundTerminalCleaner,
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
