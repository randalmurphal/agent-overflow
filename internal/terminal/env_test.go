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

// TestNormalizeTerminalEnvAppImageScrubbing covers both directions of the
// gate: an environment with no AppImage markers must pass through
// unchanged (dev, .deb, macOS, Windows-hosted WSL launches all land here,
// even when a path happens to look mount-shaped), and a marked one must
// lose the markers plus every mount-local search-path segment.
func TestNormalizeTerminalEnvAppImageScrubbing(t *testing.T) {
	const mount = "/tmp/.mount_agent1A2B3C"

	tests := []struct {
		name string
		base []string
		want []string // excluding the always-appended TERM/COLORTERM pair
	}{
		{
			name: "no markers leaves everything alone",
			base: []string{
				"PATH=" + mount + "/usr/bin:/usr/bin",
				"LD_LIBRARY_PATH=" + mount + "/usr/lib",
				"HOME=/home/dev",
			},
			want: []string{
				"PATH=" + mount + "/usr/bin:/usr/bin",
				"LD_LIBRARY_PATH=" + mount + "/usr/lib",
				"HOME=/home/dev",
			},
		},
		{
			name: "markers dropped and mount segments stripped",
			base: []string{
				"APPIMAGE=/home/dev/Apps/agent-overflow.AppImage",
				"APPDIR=" + mount,
				"ARGV0=./agent-overflow.AppImage",
				"OWD=/home/dev/projects",
				"PATH=" + mount + "/usr/bin:/usr/local/bin:" + mount + ":/usr/bin",
				"LD_LIBRARY_PATH=" + mount + "/usr/lib:/usr/lib/x86_64-linux-gnu",
				"HOME=/home/dev",
			},
			want: []string{
				"PATH=/usr/local/bin:/usr/bin",
				"LD_LIBRARY_PATH=/usr/lib/x86_64-linux-gnu",
				"HOME=/home/dev",
			},
		},
		{
			name: "segment matching is prefix-correct, not substring",
			base: []string{
				"APPDIR=" + mount,
				"PATH=" + mount + "/usr/bin" + // under the mount
					":/usr/bin" + // real system path
					":" + mount + "-backup/bin" + // sibling sharing the prefix string
					":/opt" + mount + "/bin" + // mount path embedded mid-segment
					":" + mount + "/usr/bin/" + // trailing separator, still under
					":/home/dev/.mount_agent1A2B3C", // same basename, different parent
			},
			want: []string{
				"PATH=/usr/bin:" + mount + "-backup/bin:/opt" + mount + "/bin:/home/dev/.mount_agent1A2B3C",
			},
		},
		{
			name: "search path left empty is unset rather than blanked",
			base: []string{
				"APPDIR=" + mount,
				"LD_LIBRARY_PATH=" + mount + "/usr/lib:" + mount + "/usr/lib32",
				"PATH=/usr/bin",
			},
			want: []string{"PATH=/usr/bin"},
		},
		{
			// The linuxdeploy GTK plugin prepends the mount's share dir to
			// XDG_DATA_DIRS and points GSETTINGS_SCHEMA_DIR at the mount's
			// schemas; a shell inheriting them resolves .desktop entries and
			// gsettings against a squashfs that vanishes on app exit.
			name: "GTK data and schema paths lose their mount segments",
			base: []string{
				"APPDIR=" + mount,
				"XDG_DATA_DIRS=" + mount + "/usr/share:/usr/local/share:/usr/share",
				"GSETTINGS_SCHEMA_DIR=" + mount + "/usr/share/glib-2.0/schemas",
				"PATH=/usr/bin",
			},
			want: []string{
				"XDG_DATA_DIRS=/usr/local/share:/usr/share",
				"PATH=/usr/bin",
			},
		},
		{
			name: "APPIMAGE without APPDIR drops markers but keeps paths",
			base: []string{
				"APPIMAGE=/home/dev/Apps/agent-overflow.AppImage",
				"ARGV0=./agent-overflow.AppImage",
				"PATH=" + mount + "/usr/bin:/usr/bin",
			},
			want: []string{"PATH=" + mount + "/usr/bin:/usr/bin"},
		},
		{
			name: "APPDIR at the filesystem root disables path stripping",
			base: []string{
				"APPDIR=/",
				"PATH=/usr/bin:/bin",
			},
			want: []string{"PATH=/usr/bin:/bin"},
		},
		{
			name: "empty marker values do not arm the scrub",
			base: []string{
				"APPIMAGE=",
				"APPDIR=",
				"PATH=" + mount + "/usr/bin:/usr/bin",
			},
			want: []string{
				"APPIMAGE=",
				"APPDIR=",
				"PATH=" + mount + "/usr/bin:/usr/bin",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := append(slices.Clone(tc.want), "TERM=xterm-256color", "COLORTERM=truecolor")
			got := normalizeTerminalEnv(slices.Clone(tc.base))
			if !slices.Equal(got, want) {
				t.Fatalf("normalizeTerminalEnv()\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestNormalizeTerminalEnvAppImageKeepsNonPairEntries — a malformed entry
// with no '=' is passed through untouched even under an AppImage launch;
// the scrub only ever decides about KEY=VALUE pairs it can parse.
func TestNormalizeTerminalEnvAppImageKeepsNonPairEntries(t *testing.T) {
	got := normalizeTerminalEnv([]string{"APPDIR=/tmp/.mount_x", "BROKEN_ENTRY", "PATH=/usr/bin"})
	want := []string{"BROKEN_ENTRY", "PATH=/usr/bin", "TERM=xterm-256color", "COLORTERM=truecolor"}
	if !slices.Equal(got, want) {
		t.Fatalf("normalizeTerminalEnv() = %q, want %q", got, want)
	}
}
