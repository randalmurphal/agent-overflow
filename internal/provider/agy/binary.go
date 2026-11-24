// Package agy implements the Google Antigravity (`agy acp`) provider adapter
// on top of the shared multiplexed ACP v1 client (internal/provider/acp).
package agy

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	DefaultBinaryName  = "agy"
	DefaultAppDataName = "antigravity-cli"
)

// ResolveBinary returns the configured agy binary path, falling back to PATH
// and macOS Antigravity.app bundle locations when using the default name.
func ResolveBinary(configured string) string {
	trimmed := strings.TrimSpace(configured)
	if trimmed != "" && trimmed != DefaultBinaryName {
		return trimmed
	}
	if p, err := exec.LookPath(DefaultBinaryName); err == nil && p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, candidate := range []string{
			"/Applications/Antigravity.app/Contents/Resources/app/bin/agy",
			"/usr/local/bin/agy",
			"/opt/homebrew/bin/agy",
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return DefaultBinaryName
}
