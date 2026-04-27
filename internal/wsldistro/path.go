//go:build !windows

package wsldistro

import (
	"os"
	"path/filepath"
	"strings"
)

// AppDataEnv is the env var the Windows launcher sets through WSLENV
// before spawning the Linux backend. WSLENV's /p flag translates the
// raw Windows path (e.g. C:\Users\rmurphy\AppData\Roaming) to its
// /mnt/c form on the WSL side, so by the time the backend reads it
// the value is already a Linux-rooted path that os.ReadFile / os.Stat
// accept directly.
const AppDataEnv = "AGENT_OVERFLOW_WIN_APPDATA"

// WSLConfigDir returns the WSL-side path to the launcher's wsl.json
// directory ("<AppData>/agent-overflow") and a flag that's true only
// when the backend was spawned by the Windows launcher.
//
// Returns ("", false) when:
//   - The launcher didn't set AGENT_OVERFLOW_WIN_APPDATA. That's
//     either a non-WSL host (macOS, native Linux) or a WSL backend
//     started by something other than the Windows launcher (manual
//     `go run`, a dev shell). The Settings UI uses the false return
//     to hide the WSL-distro switcher: there's no single wsl.json to
//     mutate that the launcher would ever read again.
//   - The pointed-at directory doesn't resolve. An env var leaked
//     from a different machine (image clone, env-var copy in a
//     reproducer) shouldn't trick us into writing to a non-existent
//     /mnt/c subtree.
func WSLConfigDir() (string, bool) {
	raw := os.Getenv(AppDataEnv)
	if raw == "" {
		return "", false
	}
	// Reject relative paths and any value containing a ".." segment.
	// The launcher always exports an absolute %APPDATA% path with no
	// traversal sequences; anything else means the env var leaked from
	// somewhere unexpected and a write under it could land in an
	// attacker-prepared location after filepath.Join collapses the ".."
	// (e.g. /tmp/evil/../../etc/agent-overflow → /etc/agent-overflow).
	if !filepath.IsAbs(raw) || hasTraversalSegment(raw) {
		return "", false
	}
	// Sanity-check the resolved path exists and is a real directory.
	// Anything else means the env var leaked from a different host —
	// better to fall through to "WSL settings unavailable" than to
	// write into a phantom path.
	info, err := os.Stat(raw)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return filepath.Join(raw, "agent-overflow"), true
}

// hasTraversalSegment returns true if any "/"-delimited segment of p is
// exactly "..". The launcher writes a clean absolute path on Windows
// that WSLENV /p translates into a clean /mnt/c form, so a real ".."
// segment never appears in the legitimate value.
func hasTraversalSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
