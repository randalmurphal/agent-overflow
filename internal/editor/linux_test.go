package editor

import (
	"runtime"
	"testing"
)

func TestFindLinuxInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	home := map[string]string{"HOME": "/home/test"}
	cases := []struct {
		name      string
		editorID  string
		available map[string]bool
		env       map[string]string
		want      string
	}{
		{"vscode system path", "code", map[string]bool{"/usr/share/code/bin/code": true}, home, "/usr/share/code/bin/code"},
		{"snap path", "code", map[string]bool{"/snap/bin/code": true}, home, "/snap/bin/code"},
		{"home relative", "cursor", map[string]bool{"/home/test/.local/bin/cursor": true}, home, "/home/test/.local/bin/cursor"},
		// HOME unset: the ~/-prefixed candidates are skipped without
		// crashing.
		{"home relative without HOME misses", "cursor", map[string]bool{"/home/test/.local/bin/cursor": true}, map[string]string{}, ""},
		{"unknown editor", "emacs", map[string]bool{}, home, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findLinuxInstall(tc.editorID, fakeEnvWithStat(tc.available, tc.env)); got != tc.want {
				t.Fatalf("findLinuxInstall(%s) = %q, want %q", tc.editorID, got, tc.want)
			}
		})
	}
}

func TestFindLinuxInstall_NoOpOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("only verifies the no-op contract off-linux")
	}
	env := fakeEnvWithStat(
		map[string]bool{"/usr/share/code/bin/code": true},
		map[string]string{"HOME": "/home/test"},
	)
	if got := findLinuxInstall("code", env); got != "" {
		t.Fatalf("findLinuxInstall must be a no-op off linux; got %q", got)
	}
}
