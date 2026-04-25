package editor

import (
	"runtime"
	"testing"
)

func TestFindLinuxInstall_VSCodeSystemPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/usr/share/code/bin/code": true,
		},
		map[string]string{"HOME": "/home/test"},
	)
	got := findLinuxInstall("code", env)
	if got != "/usr/share/code/bin/code" {
		t.Fatalf("findLinuxInstall(code) = %q, want /usr/share/code/bin/code", got)
	}
}

func TestFindLinuxInstall_SnapPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	env := fakeEnvWithStat(
		map[string]bool{"/snap/bin/code": true},
		map[string]string{"HOME": "/home/test"},
	)
	got := findLinuxInstall("code", env)
	if got != "/snap/bin/code" {
		t.Fatalf("findLinuxInstall(code) = %q, want /snap/bin/code", got)
	}
}

func TestFindLinuxInstall_HomeRelative(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	env := fakeEnvWithStat(
		map[string]bool{"/home/test/.local/bin/cursor": true},
		map[string]string{"HOME": "/home/test"},
	)
	got := findLinuxInstall("cursor", env)
	if got != "/home/test/.local/bin/cursor" {
		t.Fatalf("findLinuxInstall(cursor) = %q, want /home/test/.local/bin/cursor", got)
	}
}

func TestFindLinuxInstall_HomeRelativeWithoutHomeMisses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	// HOME unset — the ~/-prefixed candidates are skipped without
	// crashing. Concrete paths still work.
	env := fakeEnvWithStat(
		map[string]bool{"/home/test/.local/bin/cursor": true},
		map[string]string{},
	)
	got := findLinuxInstall("cursor", env)
	if got != "" {
		t.Fatalf("expected no match when HOME is unset; got %q", got)
	}
}

func TestFindLinuxInstall_UnknownEditor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux install table only fires on linux")
	}
	env := fakeEnvWithStat(map[string]bool{}, map[string]string{"HOME": "/home/test"})
	if got := findLinuxInstall("emacs", env); got != "" {
		t.Fatalf("unknown editor should miss; got %q", got)
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
