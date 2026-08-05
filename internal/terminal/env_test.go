//go:build !windows

package terminal

import (
	"slices"
	"testing"
)

func TestNormalizeTerminalEnvReplacesInheritedCapabilities(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("COLORTERM", "old-value")
	t.Setenv("AO_TERMINAL_ENV_TEST", "preserved")

	got := normalizeTerminalEnv(nil)
	for _, want := range []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"AO_TERMINAL_ENV_TEST=preserved",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("normalized environment missing %q", want)
		}
	}
	for _, unwanted := range []string{"TERM=dumb", "COLORTERM=old-value"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("normalized environment retained %q", unwanted)
		}
	}
}

func TestNormalizeTerminalEnvCopiesExplicitEnvironment(t *testing.T) {
	base := []string{"PATH=/custom/bin", "TERM=screen-256color", "COLORTERM=legacy"}
	got := normalizeTerminalEnv(base)

	want := []string{
		"PATH=/custom/bin",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("normalizeTerminalEnv() = %q, want %q", got, want)
	}
	if base[1] != "TERM=screen-256color" {
		t.Fatalf("normalizeTerminalEnv mutated caller input: %q", base)
	}
}

// TestNormalizeTerminalEnvAppliesTheAppImageScrub is the integration
// assertion for the shared scrub: a PTY spawn must get it, in both
// directions of its marker gate. The scrub's own semantics (segment
// matching, empty-value handling, which variables are searched) are
// covered by internal/appimage.
func TestNormalizeTerminalEnvAppliesTheAppImageScrub(t *testing.T) {
	const mount = "/tmp/.mount_agent1A2B3C"
	capabilities := []string{"TERM=xterm-256color", "COLORTERM=truecolor"}

	t.Run("markers arm it", func(t *testing.T) {
		base := []string{
			"APPIMAGE=/home/dev/Apps/agent-overflow.AppImage",
			"APPDIR=" + mount,
			"ARGV0=./agent-overflow.AppImage",
			"OWD=/home/dev/projects",
			"PATH=" + mount + "/usr/bin:/usr/local/bin:/usr/bin",
			"HOME=/home/dev",
		}
		want := append([]string{"PATH=/usr/local/bin:/usr/bin", "HOME=/home/dev"}, capabilities...)
		if got := normalizeTerminalEnv(slices.Clone(base)); !slices.Equal(got, want) {
			t.Fatalf("normalizeTerminalEnv()\n got %q\nwant %q", got, want)
		}
	})

	t.Run("no markers leaves a mount-shaped path alone", func(t *testing.T) {
		base := []string{"PATH=" + mount + "/usr/bin:/usr/bin", "HOME=/home/dev"}
		want := append(slices.Clone(base), capabilities...)
		if got := normalizeTerminalEnv(slices.Clone(base)); !slices.Equal(got, want) {
			t.Fatalf("normalizeTerminalEnv()\n got %q\nwant %q", got, want)
		}
	})
}
