//go:build windows

// config.go owns the on-disk picker state under
// %APPDATA%\agent-overflow\wsl.json. The file records the user's
// distro pick plus the payload version we last installed, so a
// subsequent launch can skip the install step when nothing changed.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// configFileName is the name of the on-disk config under
// %APPDATA%\agent-overflow\.
const configFileName = "wsl.json"

// config is the on-disk picker state. We store the chosen distro
// and the payload version we last installed so a subsequent launch
// can skip the install step when the embedded payload hasn't changed.
type config struct {
	Distro          string `json:"distro"`
	InstalledVer    string `json:"installed_version,omitempty"`
	InstalledDistro string `json:"installed_distro,omitempty"`
}

func configDir() (string, error) {
	roaming := os.Getenv("APPDATA")
	if roaming == "" {
		// Fall back to the user home if APPDATA isn't in the
		// environment (rare but possible in headless service contexts).
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate config dir: %w", err)
		}
		roaming = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(roaming, "agent-overflow"), nil
}

func loadConfig() (*config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, configFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}
	return &c, nil
}

func saveConfig(c *config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, configFileName)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return os.WriteFile(path, b, 0o644)
}
