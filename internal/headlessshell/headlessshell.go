// Package headlessshell owns the on-disk layout of the
// chrome-headless-shell cache: which Chrome-for-Testing platform this
// build is, where a downloaded version's binary sits, and how to find an
// already-installed one.
//
// It exists so two callers can agree on that layout without agreeing on
// a browser driver. `internal/screenshot` downloads into it and drives
// the binary through chromedp; `cmd/ao-harness` only wants the path of a
// shell somebody already installed, and must not link a CDP library to
// ask. Stdlib only, no network.
package headlessshell

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// CacheDirName is the directory, under an app-managed config root, that
// holds one subdirectory per installed version.
const CacheDirName = "headless-shell"

// Platform returns the Chrome-for-Testing platform string for the
// running OS+arch.
func Platform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "darwin/arm64":
		return "mac-arm64", nil
	case "windows/amd64":
		return "win64", nil
	default:
		return "", fmt.Errorf("unsupported platform %s/%s — Chrome-for-Testing has no chrome-headless-shell build", runtime.GOOS, runtime.GOARCH)
	}
}

// BinaryName is the executable's file name on the given platform.
func BinaryName(platform string) string {
	if platform == "win64" {
		return "chrome-headless-shell.exe"
	}
	return "chrome-headless-shell"
}

// BinaryPath is the canonical post-extraction path under versionDir. The
// Chrome-for-Testing zips for chrome-headless-shell always extract to a
// single subdirectory named after the platform.
func BinaryPath(versionDir, platform string) string {
	return filepath.Join(versionDir, "chrome-headless-shell-"+platform, BinaryName(platform))
}

// Executable reports whether path is a runnable file.
func Executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		// Trust filename + existence; Windows uses extension-based
		// dispatch and the zip preserves the .exe.
		return strings.HasSuffix(strings.ToLower(path), ".exe")
	}
	return info.Mode()&0o111 != 0
}

// Installed returns the path of the newest chrome-headless-shell already
// present under configDir, and the version directory's name. It never
// downloads: an empty cache is `false`, not an error, so a caller with a
// fallback chain can move on. The installer prunes to a single version,
// so "newest" normally has one candidate; it is resolved anyway because a
// prune that failed (a Windows file lock) leaves a stale sibling behind.
func Installed(configDir string) (binaryPath, version string, ok bool) {
	if strings.TrimSpace(configDir) == "" {
		return "", "", false
	}
	platform, err := Platform()
	if err != nil {
		return "", "", false
	}
	cacheDir := filepath.Join(configDir, CacheDirName)
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return "", "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := BinaryPath(filepath.Join(cacheDir, entry.Name()), platform)
		if !Executable(candidate) {
			continue
		}
		if version == "" || newerVersion(entry.Name(), version) {
			binaryPath, version = candidate, entry.Name()
		}
	}
	return binaryPath, version, version != ""
}

// newerVersion compares two dotted-decimal Chrome versions segment by
// segment. A non-numeric segment sorts below any number, so a
// hand-created directory never outranks a real install.
func newerVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := segment(as, i), segment(bs, i)
		if av != bv {
			return av > bv
		}
	}
	return false
}

func segment(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return -1
	}
	return n
}
