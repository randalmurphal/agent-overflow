//go:build windows

// config.go is the launcher's thin Windows-side wrapper over the
// shared wsl.json schema in internal/wsldistro. The launcher reads
// the file from %APPDATA%\agent-overflow\ before deciding which
// distro to boot, and writes back after a successful launch. The
// WSL-side backend reads + writes the same file through the /mnt/c
// mount when the Settings UI's distro switcher is used; centralising
// the schema and the I/O primitives in internal/wsldistro keeps the
// two sides from drifting.
package main

import (
	"agent-overflow/internal/wsldistro"
)

// loadConfig reads the launcher's wsl.json from %APPDATA%\agent-overflow.
// Returns (nil, nil) when the file doesn't exist (first launch / never
// picked a distro yet).
func loadConfig() (*wsldistro.Config, error) {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		// %APPDATA% unresolvable; treat as "no saved config" rather
		// than erroring so the picker still runs on a stripped env.
		return nil, nil
	}
	return wsldistro.Load(dir)
}

// saveConfig writes the launcher's wsl.json into %APPDATA%\agent-overflow.
func saveConfig(c *wsldistro.Config) error {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		// Without %APPDATA% we have nowhere to persist; the launcher
		// continues but the next launch won't remember the pick. The
		// picker will run again — same UX as a fresh install.
		return nil
	}
	return wsldistro.Save(dir, c)
}
