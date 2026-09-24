package supervise

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLauncherRecordPathSeparatesLaunchersAndDataRoots: dev and production
// launchers, isolated profiles and distributions each get their own record;
// names that differ only in case are one distribution; no name leaves the
// record directory.
func TestLauncherRecordPathSeparatesLaunchersAndDataRoots(t *testing.T) {
	const dir = "/config"
	seen := map[string]string{}
	for _, c := range []struct{ mode, distro string }{
		{"prod", "Ubuntu"},
		{"dev", "Ubuntu"},
		{"harness", "Ubuntu"},
		{"prod", "Debian"},
		{"prod", "Ubuntu-22.04"},
		{"prod", "Ubuntu-22%2E04"},
		{"prod", "Ubuntu.22-04"},
		{"prod", "../../x"},
		{"prod", "a/b"},
		{"prod", "a%2Fb"},
		{"prod", ""},
	} {
		path := LauncherRecordPath(dir, c.mode, c.distro)
		if filepath.Dir(path) != filepath.Join(dir, LauncherRecordDir) {
			t.Errorf("LauncherRecordPath(%q, %q) = %q, outside the record directory", c.mode, c.distro, path)
		}
		// Windows compares file names without case.
		key, name := c.mode+"/"+c.distro, strings.ToLower(path)
		if other, ok := seen[name]; ok {
			t.Errorf("%s and %s share %q", other, key, path)
		}
		seen[name] = key
	}
	if a, b := LauncherRecordPath(dir, "prod", "Ubuntu"), LauncherRecordPath(dir, "PROD", "ubuntu"); a != b {
		t.Errorf("case variants got %q and %q; Windows would treat them as one file", a, b)
	}
}
