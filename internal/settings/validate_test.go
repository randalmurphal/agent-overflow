package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
			name:  "textGenerationProvider",
			patch: map[string]any{"textGenerationProvider": "anthropic"},
		},
		{
			name:  "textGenerationReasoningEffort",
			patch: map[string]any{"textGenerationReasoningEffort": "turbo"},
		},
		{
			name:  "defaultThreadEnvMode",
			patch: map[string]any{"defaultThreadEnvMode": "remote"},
		},
		{
			name:  "paneDensity",
			patch: map[string]any{"paneDensity": "wide"},
		},
		{
			name:  "worktreeBranchPrefixSlash",
			patch: map[string]any{"worktreeBranchPrefix": "ao/"},
		},
		{
			name:  "worktreeBranchPrefixEmpty",
			patch: map[string]any{"worktreeBranchPrefix": "   "},
		},
		{
			name:  "worktreeBranchPrefixDot",
			patch: map[string]any{"worktreeBranchPrefix": ".ao-"},
		},
		{
			name:  "sansFont",
			patch: map[string]any{"sansFont": "comicsans"},
		},
		{
			name:  "monoFont",
			patch: map[string]any{"monoFont": "wingdings"},
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
  "sansFont": "comicsans",
  "monoFont": "wingdings",
  "claudeBinaryPath": " /custom/claude ",
  "codexBinaryPath": "   ",
  "defaultThreadEnvMode": "remote",
  "paneDensity": "wide",
  "worktreeBranchPrefix": "bad/prefix",
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
	if got.SansFont != DefaultSettings.SansFont {
		t.Fatalf("SansFont = %q, want %q", got.SansFont, DefaultSettings.SansFont)
	}
	if got.MonoFont != DefaultSettings.MonoFont {
		t.Fatalf("MonoFont = %q, want %q", got.MonoFont, DefaultSettings.MonoFont)
	}
	if got.ClaudeBinaryPath != "/custom/claude" {
		t.Fatalf("ClaudeBinaryPath = %q, want /custom/claude", got.ClaudeBinaryPath)
	}
	if got.CodexBinaryPath != DefaultSettings.CodexBinaryPath {
		t.Fatalf("CodexBinaryPath = %q, want %q", got.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	}
	if got.DefaultThreadEnvMode != DefaultSettings.DefaultThreadEnvMode {
		t.Fatalf("DefaultThreadEnvMode = %q, want %q", got.DefaultThreadEnvMode, DefaultSettings.DefaultThreadEnvMode)
	}
	if got.PaneDensity != DefaultSettings.PaneDensity {
		t.Fatalf("PaneDensity = %q, want %q", got.PaneDensity, DefaultSettings.PaneDensity)
	}
	if got.WorktreeBranchPrefix != DefaultSettings.WorktreeBranchPrefix {
		t.Fatalf("WorktreeBranchPrefix = %q, want %q", got.WorktreeBranchPrefix, DefaultSettings.WorktreeBranchPrefix)
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

func TestTextGenerationReasoningEffortIsProviderSpecific(t *testing.T) {
	svc := NewService(t.TempDir())

	if _, err := svc.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationReasoningEffort": "minimal",
	}); err != nil {
		t.Fatalf("codex minimal effort should be valid: %v", err)
	}

	if _, err := svc.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationReasoningEffort": "max",
	}); err == nil {
		t.Fatal("codex max effort should be invalid")
	}

	if _, err := svc.Update(map[string]any{
		"textGenerationProvider":        "claude",
		"textGenerationReasoningEffort": "max",
	}); err != nil {
		t.Fatalf("claude max effort should be valid: %v", err)
	}

	if _, err := svc.Update(map[string]any{
		"textGenerationProvider":        "claude",
		"textGenerationReasoningEffort": "minimal",
	}); err == nil {
		t.Fatal("claude minimal effort should be invalid")
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

func TestRetentionDefaultIsThirtyDays(t *testing.T) {
	got := NewService(t.TempDir()).Get()
	if got.Retention.Days != 30 {
		t.Fatalf("Retention.Days default = %d, want 30", got.Retention.Days)
	}
}

func TestRetentionUpdateRoundTrip(t *testing.T) {
	svc := NewService(t.TempDir())

	// Zero is legal (disables the sweep).
	got, err := svc.Update(map[string]any{
		"retention": map[string]any{"days": 0},
	})
	if err != nil {
		t.Fatalf("retention.days=0: %v", err)
	}
	if got.Retention.Days != 0 {
		t.Fatalf("retention.days=0 round-trip: got %d, want 0", got.Retention.Days)
	}

	// Custom positive value round-trips.
	got, err = svc.Update(map[string]any{
		"retention": map[string]any{"days": 7},
	})
	if err != nil {
		t.Fatalf("retention.days=7: %v", err)
	}
	if got.Retention.Days != 7 {
		t.Fatalf("retention.days=7 round-trip: got %d, want 7", got.Retention.Days)
	}
}

func TestRetentionRejectsNegativeOnWrite(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.Update(map[string]any{
		"retention": map[string]any{"days": -5},
	}); err == nil {
		t.Fatal("Update(retention.days=-5) should fail validation")
	}
}

func TestRetentionSanitizesNegativeOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"retention": {"days": -3}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := NewService(dir).Get()
	if got.Retention.Days != 0 {
		t.Fatalf("Retention.Days = %d, want 0 (clamped from -3)", got.Retention.Days)
	}
}

func TestRetentionRejectsOverflowingDaysOnWrite(t *testing.T) {
	svc := NewService(t.TempDir())
	// Above MaxRetentionDays so the Duration math downstream stays
	// well-defined regardless of user input.
	if _, err := svc.Update(map[string]any{
		"retention": map[string]any{"days": MaxRetentionDays + 1},
	}); err == nil {
		t.Fatal("Update(retention.days > MaxRetentionDays) should fail validation")
	}
}

func TestRetentionClampsOverflowingDaysOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"retention": {"days": 999999999}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := NewService(dir).Get()
	if got.Retention.Days != MaxRetentionDays {
		t.Fatalf("Retention.Days = %d, want %d (clamped from 999999999)", got.Retention.Days, MaxRetentionDays)
	}
}

func TestGitLabSelfHostedHostsUpdateNormalisesEntries(t *testing.T) {
	svc := NewService(t.TempDir())

	// Empty entries are rejected on the strict (Update) path; the load
	// path drops them silently. Confirm the reject before checking the
	// happy-path normalisation.
	updated, err := svc.Update(map[string]any{
		"gitlabSelfHostedHosts": []string{
			"gitlab.mycompany.com",
			"",
		},
	})
	if err == nil {
		t.Fatalf("Update with empty host entry should fail, got %+v", updated)
	}

	// Clean list round-trips with normalisation (trim + lowercase +
	// dedupe).
	got, err := svc.Update(map[string]any{
		"gitlabSelfHostedHosts": []string{
			"  Gitlab.MyCompany.com  ",
			"gl.example.test",
			"gitlab.mycompany.com",
		},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	want := []string{"gitlab.mycompany.com", "gl.example.test"}
	if len(got.GitLabSelfHostedHosts) != len(want) {
		t.Fatalf("GitLabSelfHostedHosts = %v, want %v", got.GitLabSelfHostedHosts, want)
	}
	for i, h := range want {
		if got.GitLabSelfHostedHosts[i] != h {
			t.Errorf("GitLabSelfHostedHosts[%d] = %q, want %q", i, got.GitLabSelfHostedHosts[i], h)
		}
	}
}

func TestGitLabSelfHostedHostsRejectsBadInputs(t *testing.T) {
	svc := NewService(t.TempDir())
	cases := []struct {
		name  string
		hosts []string
	}{
		{"scheme prefix", []string{"https://gitlab.example.com"}},
		{"trailing path", []string{"gitlab.example.com/group/repo"}},
		{"with port", []string{"gitlab.example.com:443"}},
		{"with userinfo", []string{"git@gitlab.example.com"}},
		{"contains space", []string{"gitlab example.com"}},
		{"single-label (no dot)", []string{"localhost"}},
		{"leading dot", []string{".gitlab.example.com"}},
		{"trailing dot", []string{"gitlab.example.com."}},
		{"double dot", []string{"gitlab..example.com"}},
		{"hyphen edge", []string{"-gitlab.example.com"}},
		{"redundant github.com", []string{"github.com"}},
		{"redundant gitlab.com", []string{"GitLab.com"}},
		{"invalid char", []string{"gitlab_example.com"}},
		{"label too long", []string{strings.Repeat("a", 64) + ".example.com"}},
		{"hostname too long", []string{strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Update(map[string]any{
				"gitlabSelfHostedHosts": tc.hosts,
			}); err == nil {
				t.Fatalf("Update(%v) error = nil, want validation failure", tc.hosts)
			}
		})
	}
}

func TestGitLabSelfHostedHostsRejectsTooMany(t *testing.T) {
	svc := NewService(t.TempDir())
	hosts := make([]string, MaxGitLabSelfHostedHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host-%d.example.com", i)
	}
	if _, err := svc.Update(map[string]any{"gitlabSelfHostedHosts": hosts}); err == nil {
		t.Fatalf("Update with %d hosts should fail", len(hosts))
	}
}

func TestGitLabSelfHostedHostsSanitizesOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	data := []byte(`{
  "gitlabSelfHostedHosts": [
    "  Gitlab.Example.com  ",
    "",
    "https://other.example.com",
    "gitlab.example.com",
    "github.com",
    "ok.example.com"
  ]
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := NewService(dir).Get()
	want := []string{"gitlab.example.com", "ok.example.com"}
	if len(got.GitLabSelfHostedHosts) != len(want) {
		t.Fatalf("GitLabSelfHostedHosts = %v, want %v", got.GitLabSelfHostedHosts, want)
	}
	for i, h := range want {
		if got.GitLabSelfHostedHosts[i] != h {
			t.Errorf("[%d] = %q, want %q", i, got.GitLabSelfHostedHosts[i], h)
		}
	}
}

func TestGitLabSelfHostedHostsDefaultIsNil(t *testing.T) {
	got := NewService(t.TempDir()).Get()
	if got.GitLabSelfHostedHosts != nil {
		t.Fatalf("GitLabSelfHostedHosts default = %v, want nil", got.GitLabSelfHostedHosts)
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
