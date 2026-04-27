//go:build windows

package wsldistro

import (
	"os"
	"path/filepath"
)

// AppDataEnv is the env var the Windows launcher sets through WSLENV
// before spawning the Linux backend. The constant is shared between
// platforms so the launcher's setup code and the WSL-side reader
// agree on the name; on Windows the value is read directly from the
// process environment without WSLENV translation.
const AppDataEnv = "AGENT_OVERFLOW_WIN_APPDATA"

// WSLConfigDir returns the Windows-side path to the launcher's
// wsl.json directory (%APPDATA%\agent-overflow) and a flag that's
// true when %APPDATA% resolves.
//
// On Windows the launcher itself is the consumer of this directory —
// the same path used by loadConfig / saveConfig in
// cmd/agent-overflow-windows/config.go. Centralising it here keeps a
// single source of truth so the launcher's saved-config reads and
// the WSL backend's writes can never disagree on the directory name.
func WSLConfigDir() (string, bool) {
	roaming := os.Getenv("APPDATA")
	if roaming == "" {
		// Fall back to the user home if APPDATA isn't in the
		// environment (rare but possible in headless service
		// contexts that strip env vars). Mirrors the launcher's
		// pre-extraction behavior so a deployment that worked
		// before keeps working.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		roaming = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(roaming, "agent-overflow"), true
}
