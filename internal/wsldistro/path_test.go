//go:build !windows

package wsldistro

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWSLConfigDir covers the AppData export: honoured only when it names an
// existing absolute directory, and returned joined with the app's
// subdirectory. The ".." segment rule is shared with WindowsDownloadsDir and
// is covered by that test's traversal row.
func TestWSLConfigDir(t *testing.T) {
	existing := t.TempDir()
	// A regular file must not be accepted: Save would otherwise try to create
	// a child path under a non-directory and fail mid-write.
	regular := filepath.Join(existing, "not-a-dir")
	if err := os.WriteFile(regular, []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cases := []struct {
		name, value, want string
		ok                bool
	}{
		{"unset", "", "", false},
		{"existing directory", existing, filepath.Join(existing, "agent-overflow"), true},
		{"missing path", "/this/path/should/not/exist/anywhere", "", false},
		// A relative value would resolve against the backend's CWD, a different
		// trust surface from %APPDATA%; the launcher never produces it.
		{"relative path", "tmp/relative", "", false},
		{"regular file", regular, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(AppDataEnv, tc.value)
			got, ok := WSLConfigDir()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("WSLConfigDir() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestWindowsDownloadsDir covers the Downloads export under the same
// rules as the AppData one: honoured only when it names an existing
// absolute directory, and returned as-is rather than joined.
func TestWindowsDownloadsDir(t *testing.T) {
	existing := t.TempDir()
	regular := filepath.Join(existing, "not-a-dir")
	if err := os.WriteFile(regular, []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cases := []struct {
		name, value, want string
		ok                bool
	}{
		{"unset", "", "", false},
		{"existing directory", existing, existing, true},
		{"missing path", filepath.Join(existing, "missing"), "", false},
		{"regular file", regular, "", false},
		{"relative path", "mnt/c/Users/u/Downloads", "", false},
		// Resolves to the existing directory once cleaned, so only the
		// segment check can refuse it.
		{"traversal", existing + "/../" + filepath.Base(existing), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(DownloadsEnv, tc.value)
			got, ok := WindowsDownloadsDir()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("WindowsDownloadsDir() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
