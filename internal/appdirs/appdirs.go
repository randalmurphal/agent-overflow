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

// The two modes every file and directory Agent Overflow creates under Root()
// is given. They live here rather than on the App because the app-managed tree
// has one owner and one privacy rule, and the packages that write into it
// (`main`, the workflow runner) must not each pick their own.
const (
	// PrivateDirPerm is owner-only for a directory the app manages.
	PrivateDirPerm os.FileMode = 0o700
	// SensitiveFilePerm is owner-only for a file whose contents are the user's
	// — provider prose, run narratives, captured artifacts, credentials.
	SensitiveFilePerm os.FileMode = 0o600
)
