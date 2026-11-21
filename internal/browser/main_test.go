package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain runs the package's tests in a temporary home, so a download
// saved to the user's Downloads folder (the fake engine's default, as the
// desktop engines') lands in that home's and never the developer's. The
// gated native engine smoke sets up its own (nativeSmokeMain).
func TestMain(m *testing.M) {
	if code, ran := nativeSmokeMain(m); ran {
		os.Exit(code)
	}
	os.Exit(runInTestHome(m))
}

func runInTestHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "ao-browser-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "browser tests: %v\n", err)
		return 1
	}
	code := 1
	if err := useTestHome(home); err != nil {
		fmt.Fprintf(os.Stderr, "browser tests: %v\n", err)
	} else {
		code = m.Run()
	}
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "browser tests: remove %s: %v\n", home, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// useTestHome points this process's home and XDG folders into home and
// gives it an empty Downloads folder (userDownloadsDir).
func useTestHome(home string) error {
	for name, dir := range map[string]string{
		"HOME": home, "XDG_DATA_HOME": filepath.Join(home, "data"),
		"XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_CONFIG_HOME": filepath.Join(home, "config"),
	} {
		if err := os.Setenv(name, dir); err != nil {
			return err
		}
	}
	return os.Mkdir(filepath.Join(home, "Downloads"), 0o700)
}
