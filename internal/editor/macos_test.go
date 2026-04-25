package editor

import (
	"runtime"
	"testing"
)

// fakeEnvWithStat builds a detectEnv whose stat fn answers "true" for
// any path in available and "false" otherwise. lookPath always misses
// so the install-table fallback is exercised.
func fakeEnvWithStat(available map[string]bool, env map[string]string) detectEnv {
	return detectEnv{
		lookPath: func(name string) (string, bool) { return "", false },
		readFile: func(path string) ([]byte, error) { return nil, nil },
		readDir:  func(path string) ([]string, error) { return nil, nil },
		stat: func(path string) (bool, error) {
			return available[path], nil
		},
		envValue: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
	}
}

func TestFindMacOSInstall_VSCodeSystemBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code": true,
		},
		map[string]string{"HOME": "/Users/test"},
	)
	got := findMacOSInstall("code", env)
	want := "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"
	if got != want {
		t.Fatalf("findMacOSInstall(code) = %q, want %q", got, want)
	}
}

func TestFindMacOSInstall_PrefersSystemOverHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/Applications/Cursor.app/Contents/Resources/app/bin/cursor":            true,
			"/Users/test/Applications/Cursor.app/Contents/Resources/app/bin/cursor": true,
		},
		map[string]string{"HOME": "/Users/test"},
	)
	got := findMacOSInstall("cursor", env)
	if got != "/Applications/Cursor.app/Contents/Resources/app/bin/cursor" {
		t.Fatalf("system-wide bundle should win over user-local; got %q", got)
	}
}

func TestFindMacOSInstall_FallsBackToUserApplications(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/Users/test/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code": true,
		},
		map[string]string{"HOME": "/Users/test"},
	)
	got := findMacOSInstall("code", env)
	if got != "/Users/test/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" {
		t.Fatalf("expected ~/Applications fallback; got %q", got)
	}
}

func TestFindMacOSInstall_UnknownEditor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	env := fakeEnvWithStat(map[string]bool{}, map[string]string{"HOME": "/Users/test"})
	if got := findMacOSInstall("emacs", env); got != "" {
		t.Fatalf("unknown editor should miss; got %q", got)
	}
}

func TestFindMacOSInstall_NoOpOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("only verifies the no-op contract off-darwin")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code": true,
		},
		map[string]string{"HOME": "/Users/test"},
	)
	if got := findMacOSInstall("code", env); got != "" {
		t.Fatalf("findMacOSInstall must be a no-op off darwin; got %q", got)
	}
}

func TestFindMacOSInstall_ZedPreviewBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	env := fakeEnvWithStat(
		map[string]bool{
			"/Applications/Zed Preview.app/Contents/MacOS/cli": true,
		},
		map[string]string{"HOME": "/Users/test"},
	)
	got := findMacOSInstall("zed", env)
	if got != "/Applications/Zed Preview.app/Contents/MacOS/cli" {
		t.Fatalf("Zed Preview bundle should be reachable when stable Zed isn't installed; got %q", got)
	}
}
