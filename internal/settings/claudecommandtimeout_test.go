package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// 2 h is the CLI's own background floor, so anything lower is a save that
// changes nothing; past a week the value is a typo, not a command.
func TestValidateClaudeMaxCommandTimeoutHoursBounds(t *testing.T) {
	for _, ok := range []int{MinClaudeMaxCommandTimeoutHours, DefaultClaudeMaxCommandTimeoutHours, MaxClaudeMaxCommandTimeoutHours} {
		if err := validateClaudeMaxCommandTimeoutHours("claudeMaxCommandTimeoutHours", ok); err != nil {
			t.Fatalf("validate(%d) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{0, -1, 1, MaxClaudeMaxCommandTimeoutHours + 1} {
		if err := validateClaudeMaxCommandTimeoutHours("claudeMaxCommandTimeoutHours", bad); err == nil {
			t.Fatalf("validate(%d) = nil error, want refusal", bad)
		}
	}
	bad := DefaultSettings
	bad.ClaudeMaxCommandTimeoutHours = 1
	if _, err := validateSettings(bad); err == nil {
		t.Fatal("validateSettings accepted a 1 h ceiling")
	}
}

// The ceiling must stay under the CLI's own clamp (2147483647 ms), or the
// stored value would differ from the one Claude applies.
func TestClaudeMaxCommandTimeoutCeilingFitsTheCLIClamp(t *testing.T) {
	if ms := int64(MaxClaudeMaxCommandTimeoutHours) * 3_600_000; ms > 2147483647 {
		t.Fatalf("max %d h = %d ms, over the CLI clamp", MaxClaudeMaxCommandTimeoutHours, ms)
	}
}

func TestSanitizeClaudeMaxCommandTimeoutHoursClamps(t *testing.T) {
	cases := map[int]int{0: 2, 1: 2, 2: 2, 5: 5, 168: 168, 1000: 168}
	for in, want := range cases {
		if got := sanitizeClaudeMaxCommandTimeoutHours("claudeMaxCommandTimeoutHours", in); got != want {
			t.Fatalf("sanitize(%d) = %d, want %d", in, got, want)
		}
	}
}

// AO always exports a ceiling, so a settings file that predates the key
// must read as the 5 h default, a refused save must not land, and a saved
// value must survive reload.
func TestClaudeMaxCommandTimeoutDefaultsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"claudeEnabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(dir)
	if got := svc.Get().ClaudeMaxCommandTimeoutHours; got != 5 {
		t.Fatalf("ClaudeMaxCommandTimeoutHours = %d for a file without the key, want 5", got)
	}
	if _, err := svc.Update(map[string]any{"claudeMaxCommandTimeoutHours": 1}); err == nil {
		t.Fatal("Update accepted 1 h")
	}
	if got := svc.Get().ClaudeMaxCommandTimeoutHours; got != 5 {
		t.Fatalf("ClaudeMaxCommandTimeoutHours = %d after a refused save, want 5", got)
	}
	if _, err := svc.Update(map[string]any{"claudeMaxCommandTimeoutHours": 12}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := NewService(dir).Get().ClaudeMaxCommandTimeoutHours; got != 12 {
		t.Fatalf("reloaded ClaudeMaxCommandTimeoutHours = %d, want 12", got)
	}
}

// A hand-edited file below the floor loads clamped instead of failing.
func TestClaudeMaxCommandTimeoutLoadClampsAnOutOfRangeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"claudeMaxCommandTimeoutHours":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := NewService(dir).Get().ClaudeMaxCommandTimeoutHours; got != MinClaudeMaxCommandTimeoutHours {
		t.Fatalf("loaded ClaudeMaxCommandTimeoutHours = %d, want %d", got, MinClaudeMaxCommandTimeoutHours)
	}
}

// One binary: claude-tui reads the same variable. Codex has none.
func TestMaxCommandTimeoutHoursForProvider(t *testing.T) {
	s := Settings{ClaudeMaxCommandTimeoutHours: 7}
	for _, p := range []string{"claude", "claude-tui"} {
		if got := s.MaxCommandTimeoutHoursForProvider(p); got != 7 {
			t.Fatalf("MaxCommandTimeoutHoursForProvider(%q) = %d, want 7", p, got)
		}
	}
	for _, p := range []string{"codex", ""} {
		if got := s.MaxCommandTimeoutHoursForProvider(p); got != 0 {
			t.Fatalf("MaxCommandTimeoutHoursForProvider(%q) = %d, want 0", p, got)
		}
	}
}
