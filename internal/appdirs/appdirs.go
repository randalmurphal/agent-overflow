// Package appdirs owns the one fallback chain that locates Agent
// Overflow's app-managed directory root. main.go's boot-time settings
// reads and the offline `ao` CLI must resolve the SAME directory the
// App later reads and writes, so the chain lives here instead of being
// mirrored per caller. Note os.UserConfigDir() deliberately ignores XDG
// on macOS — keep that quirk in one place.
package appdirs

import (
	"fmt"
	"os"
	"path/filepath"
)

// DirName is the app-managed directory appended to the resolved base.
const DirName = "agent-overflow"

// Root resolves <base>/agent-overflow where base is os.UserConfigDir(),
// falling back to os.UserHomeDir(). It returns an error only when
// neither base resolves; callers choose whether that is fatal (the ao
// CLI) or means "no persisted preference" (boot-time settings reads).
func Root() (string, error) {
	base, configErr := os.UserConfigDir()
	if configErr != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", fmt.Errorf("cannot determine config directory: %w (home dir also unavailable: %v)", configErr, homeErr)
		}
		base = home
	}
	return filepath.Join(base, DirName), nil
}
