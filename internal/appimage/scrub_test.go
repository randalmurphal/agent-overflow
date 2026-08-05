package appimage

import (
	"os"
	"slices"
	"testing"
)

const mount = "/tmp/.mount_agent1A2B3C"

// TestScrub covers both directions of the marker gate: an environment
// with no AppImage markers must pass through unchanged (dev, .deb, macOS,
// Windows-hosted WSL launches all land here, even when a path happens to
// look mount-shaped), and a marked one must lose the markers plus every
// mount-local search-path segment.
func TestScrub(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "no markers leaves everything alone",
			env: []string{
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
			env: []string{
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
			env: []string{
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
			env: []string{
				"APPDIR=" + mount,
				"LD_LIBRARY_PATH=" + mount + "/usr/lib:" + mount + "/usr/lib32",
				"PATH=/usr/bin",
			},
			want: []string{"PATH=/usr/bin"},
		},
		{
			// The linuxdeploy GTK plugin prepends the mount's share dir to
			// XDG_DATA_DIRS and points GSETTINGS_SCHEMA_DIR at the mount's
			// schemas; a child inheriting them resolves .desktop entries and
			// gsettings against a squashfs that vanishes on app exit.
			name: "GTK data and schema paths lose their mount segments",
			env: []string{
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
			env: []string{
				"APPIMAGE=/home/dev/Apps/agent-overflow.AppImage",
				"ARGV0=./agent-overflow.AppImage",
				"PATH=" + mount + "/usr/bin:/usr/bin",
			},
			want: []string{"PATH=" + mount + "/usr/bin:/usr/bin"},
		},
		{
			name: "APPDIR at the filesystem root disables path stripping",
			env: []string{
				"APPDIR=/",
				"PATH=/usr/bin:/bin",
			},
			want: []string{"PATH=/usr/bin:/bin"},
		},
		{
			name: "APPDIR with a trailing separator still matches",
			env: []string{
				"APPDIR=" + mount + "/",
				"PATH=" + mount + "/usr/bin:/usr/bin",
			},
			want: []string{"PATH=/usr/bin"},
		},
		{
			name: "empty marker values do not arm the scrub",
			env: []string{
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
		{
			name: "empty search-path values survive an armed scrub",
			env: []string{
				"APPDIR=" + mount,
				"PATH=",
				"LD_LIBRARY_PATH=",
			},
			want: []string{"PATH=", "LD_LIBRARY_PATH="},
		},
		{
			name: "entries that are not KEY=VALUE pairs pass through",
			env:  []string{"APPDIR=" + mount, "BROKEN_ENTRY", "PATH=" + mount + "/bin:/usr/bin"},
			want: []string{"BROKEN_ENTRY", "PATH=/usr/bin"},
		},
		{
			name: "an empty environment is not an AppImage launch",
			env:  []string{},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := slices.Clone(tc.env)
			got := Scrub(input)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Scrub()\n got %q\nwant %q", got, tc.want)
			}
			if !slices.Equal(input, tc.env) {
				t.Fatalf("Scrub mutated its input: got %q, want %q", input, tc.env)
			}
		})
	}
}

// TestScrubIsIdempotent — a second pass has no markers left to find, so
// applying the scrub at a spawn site whose environment was already
// scrubbed upstream cannot degrade it.
func TestScrubIsIdempotent(t *testing.T) {
	env := []string{
		"APPDIR=" + mount,
		"APPIMAGE=/home/dev/Apps/agent-overflow.AppImage",
		"PATH=" + mount + "/usr/bin:/usr/bin",
		"HOME=/home/dev",
	}
	once := Scrub(env)
	twice := Scrub(once)
	if !slices.Equal(once, twice) {
		t.Fatalf("second Scrub changed the result:\n once %q\ntwice %q", once, twice)
	}
}

// TestScrubNilEnvironment guards the nil case callers hit when they hand
// over an unset environment slice.
func TestScrubNilEnvironment(t *testing.T) {
	if got := Scrub(nil); got != nil {
		t.Fatalf("Scrub(nil) = %q, want nil", got)
	}
}

func TestScrubInheritedReturnsNilWithoutMarkers(t *testing.T) {
	for _, key := range markers {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Setenv("PATH", mount+"/usr/bin:/usr/bin")

	if got := ScrubInherited(); got != nil {
		t.Fatalf("ScrubInherited() = %q, want nil for a non-AppImage launch", got)
	}
}

func TestScrubInheritedScrubsTheProcessEnvironment(t *testing.T) {
	t.Setenv("APPDIR", mount)
	t.Setenv("APPIMAGE", "/home/dev/Apps/agent-overflow.AppImage")
	t.Setenv("ARGV0", "./agent-overflow.AppImage")
	t.Setenv("OWD", "/home/dev/projects")
	t.Setenv("PATH", mount+"/usr/bin:/usr/bin")
	t.Setenv("AO_APPIMAGE_SCRUB_TEST", "preserved")

	got := ScrubInherited()
	if got == nil {
		t.Fatal("ScrubInherited() = nil, want a scrubbed environment")
	}
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Errorf("ScrubInherited() did not strip the mount from PATH: %q", got)
	}
	if !slices.Contains(got, "AO_APPIMAGE_SCRUB_TEST=preserved") {
		t.Errorf("ScrubInherited() dropped an unrelated variable: %q", got)
	}
	for _, key := range markers {
		if slices.ContainsFunc(got, func(entry string) bool {
			return len(entry) > len(key) && entry[:len(key)+1] == key+"="
		}) {
			t.Errorf("ScrubInherited() retained the %s marker: %q", key, got)
		}
	}
}
