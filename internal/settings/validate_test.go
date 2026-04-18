package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateRejectsInvalidEnumeratedValues(t *testing.T) {
	svc := NewService(t.TempDir())

	testCases := []struct {
		name  string
		patch map[string]any
	}{
		{
			name:  "theme",
			patch: map[string]any{"theme": "solarized"},
		},
		{
			name:  "timestampFormat",
			patch: map[string]any{"timestampFormat": "iso8601"},
		},
		{
			name:  "defaultProvider",
			patch: map[string]any{"defaultProvider": "openai"},
		},
		{
			name:  "defaultModelClaude",
			patch: map[string]any{"defaultModelClaude": "   "},
		},
		{
			name:  "defaultModelCodex",
			patch: map[string]any{"defaultModelCodex": ""},
		},
		{
			name:  "textGenerationProvider",
			patch: map[string]any{"textGenerationProvider": "anthropic"},
		},
		{
			name:  "textGenerationReasoningEffort",
			patch: map[string]any{"textGenerationReasoningEffort": "turbo"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Update(tc.patch); err == nil {
				t.Fatalf("Update(%v) error = nil, want validation failure", tc.patch)
			}
		})
	}
}

func TestGetSanitizesInvalidLoadedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	data := []byte(`{
  "theme": "solarized",
  "timestampFormat": "iso8601",
  "defaultProvider": "openai",
  "defaultModelClaude": "   ",
  "defaultModelCodex": "",
  "claudeBinaryPath": " /custom/claude ",
  "codexBinaryPath": "   ",
  "recentWorkspaces": ["", " /tmp/one ", "/tmp/one", "/tmp/two"]
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := NewService(dir).Get()

	if got.Theme != DefaultSettings.Theme {
		t.Fatalf("Theme = %q, want %q", got.Theme, DefaultSettings.Theme)
	}
	if got.TimestampFormat != DefaultSettings.TimestampFormat {
		t.Fatalf("TimestampFormat = %q, want %q", got.TimestampFormat, DefaultSettings.TimestampFormat)
	}
	if got.DefaultProvider != DefaultSettings.DefaultProvider {
		t.Fatalf("DefaultProvider = %q, want %q", got.DefaultProvider, DefaultSettings.DefaultProvider)
	}
	if got.DefaultModelClaude != DefaultSettings.DefaultModelClaude {
		t.Fatalf("DefaultModelClaude = %q, want %q", got.DefaultModelClaude, DefaultSettings.DefaultModelClaude)
	}
	if got.DefaultModelCodex != DefaultSettings.DefaultModelCodex {
		t.Fatalf("DefaultModelCodex = %q, want %q", got.DefaultModelCodex, DefaultSettings.DefaultModelCodex)
	}
	if got.ClaudeBinaryPath != "/custom/claude" {
		t.Fatalf("ClaudeBinaryPath = %q, want /custom/claude", got.ClaudeBinaryPath)
	}
	if got.CodexBinaryPath != DefaultSettings.CodexBinaryPath {
		t.Fatalf("CodexBinaryPath = %q, want %q", got.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	}
	if len(got.RecentWorkspaces) != 2 {
		t.Fatalf("len(RecentWorkspaces) = %d, want 2", len(got.RecentWorkspaces))
	}
	if got.RecentWorkspaces[0] != "/tmp/one" || got.RecentWorkspaces[1] != "/tmp/two" {
		t.Fatalf("RecentWorkspaces = %v, want [/tmp/one /tmp/two]", got.RecentWorkspaces)
	}
}

func TestUpdateNormalizesRecentWorkspaces(t *testing.T) {
	svc := NewService(t.TempDir())

	updated, err := svc.Update(map[string]any{
		"recentWorkspaces": []string{
			"",
			" /tmp/one ",
			"/tmp/one",
			"/tmp/two",
			"/tmp/three",
			"/tmp/four",
			"/tmp/five",
			"/tmp/six",
			"/tmp/seven",
			"/tmp/eight",
			"/tmp/nine",
			"/tmp/ten",
			"/tmp/eleven",
		},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if len(updated.RecentWorkspaces) != 10 {
		t.Fatalf("len(RecentWorkspaces) = %d, want 10", len(updated.RecentWorkspaces))
	}
	if updated.RecentWorkspaces[0] != "/tmp/one" {
		t.Fatalf("RecentWorkspaces[0] = %q, want /tmp/one", updated.RecentWorkspaces[0])
	}
	if updated.RecentWorkspaces[9] != "/tmp/ten" {
		t.Fatalf("RecentWorkspaces[9] = %q, want /tmp/ten", updated.RecentWorkspaces[9])
	}
}

func TestAddRecentWorkspaceIgnoresEmptyPaths(t *testing.T) {
	svc := NewService(t.TempDir())

	svc.AddRecentWorkspace("")
	svc.AddRecentWorkspace("   ")

	if got := svc.Get(); len(got.RecentWorkspaces) != 0 {
		t.Fatalf("RecentWorkspaces = %v, want empty list", got.RecentWorkspaces)
	}
}

func TestTextGenerationDefaultsAndRoundTrip(t *testing.T) {
	svc := NewService(t.TempDir())
	got := svc.Get()
	if got.TextGenerationProvider != "codex" {
		t.Fatalf("TextGenerationProvider default = %q, want codex", got.TextGenerationProvider)
	}
	if got.TextGenerationModel != "" {
		t.Fatalf("TextGenerationModel default = %q, want empty", got.TextGenerationModel)
	}
	if got.TextGenerationReasoningEffort != "low" {
		t.Fatalf("TextGenerationReasoningEffort default = %q, want low", got.TextGenerationReasoningEffort)
	}

	updated, err := svc.Update(map[string]any{
		"textGenerationProvider":        "claude",
		"textGenerationModel":           "  claude-haiku-4-5  ",
		"textGenerationReasoningEffort": "medium",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.TextGenerationProvider != "claude" {
		t.Errorf("provider round-trip: got %q", updated.TextGenerationProvider)
	}
	if updated.TextGenerationModel != "claude-haiku-4-5" {
		t.Errorf("model trim: got %q", updated.TextGenerationModel)
	}
	if updated.TextGenerationReasoningEffort != "medium" {
		t.Errorf("effort round-trip: got %q", updated.TextGenerationReasoningEffort)
	}
}

func TestTextGenerationSanitizesInvalidOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{
  "textGenerationProvider": "openai",
  "textGenerationReasoningEffort": "turbo"
}
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := NewService(dir).Get()
	if got.TextGenerationProvider != DefaultSettings.TextGenerationProvider {
		t.Errorf("provider = %q, want default %q", got.TextGenerationProvider, DefaultSettings.TextGenerationProvider)
	}
	if got.TextGenerationReasoningEffort != DefaultSettings.TextGenerationReasoningEffort {
		t.Errorf("effort = %q, want default %q", got.TextGenerationReasoningEffort, DefaultSettings.TextGenerationReasoningEffort)
	}
}

func TestUpdateDefaultsBlankBinaryPaths(t *testing.T) {
	svc := NewService(t.TempDir())

	got, err := svc.Update(map[string]any{
		"claudeBinaryPath": "   ",
		"codexBinaryPath":  "",
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if got.ClaudeBinaryPath != DefaultSettings.ClaudeBinaryPath {
		t.Fatalf("ClaudeBinaryPath = %q, want %q", got.ClaudeBinaryPath, DefaultSettings.ClaudeBinaryPath)
	}
	if got.CodexBinaryPath != DefaultSettings.CodexBinaryPath {
		t.Fatalf("CodexBinaryPath = %q, want %q", got.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	}
}
