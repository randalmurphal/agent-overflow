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

func TestFindMacOSInstall(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS install table only fires on darwin")
	}
	const (
		systemCode = "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"
		userCode   = "/Users/test/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"
		zedPreview = "/Applications/Zed Preview.app/Contents/MacOS/cli"
	)
	cases := []struct {
		name      string
		editorID  string
		available map[string]bool
		want      string
	}{
		{"vscode system bundle", "code", map[string]bool{systemCode: true}, systemCode},
		{
			"system bundle wins over user-local", "cursor",
			map[string]bool{
				"/Applications/Cursor.app/Contents/Resources/app/bin/cursor":            true,
				"/Users/test/Applications/Cursor.app/Contents/Resources/app/bin/cursor": true,
			},
			"/Applications/Cursor.app/Contents/Resources/app/bin/cursor",
		},
		{"falls back to ~/Applications", "code", map[string]bool{userCode: true}, userCode},
		{"unknown editor", "emacs", map[string]bool{}, ""},
		// Zed Preview is reachable when stable Zed isn't installed.
		{"zed preview bundle", "zed", map[string]bool{zedPreview: true}, zedPreview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := fakeEnvWithStat(tc.available, map[string]string{"HOME": "/Users/test"})
			if got := findMacOSInstall(tc.editorID, env); got != tc.want {
				t.Fatalf("findMacOSInstall(%s) = %q, want %q", tc.editorID, got, tc.want)
			}
		})
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
