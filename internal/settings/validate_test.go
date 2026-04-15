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
