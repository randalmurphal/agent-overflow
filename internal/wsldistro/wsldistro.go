package wsldistro

import (
	"errors"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
)

// FileName is the on-disk name of the launcher config under
// %APPDATA%\agent-overflow\ (Windows) or its /mnt/c mirror (WSL).
const FileName = "wsl.json"

// Config is the on-disk picker state. The schema is shared between
// the launcher (which records installation fields after a successful boot)
// and the WSL backend (which mutates only Distro for the Settings picker).
type Config struct {
	// Distro is the WSL distribution name the launcher should boot
	// into on the next launch.
	Distro string `json:"distro"`

	// InstalledVer is the payloadVersion stamped onto the launcher
	// at build time, recorded after a successful install + boot. The
	// value is display metadata, not a cache key: distinct builds can share it.
	InstalledVer string `json:"installed_version,omitempty"`

	// InstalledSHA256 identifies the exact embedded Linux payload last installed.
	// Empty legacy records reinstall once rather than trusting a version label.
	InstalledSHA256 string `json:"installed_sha256,omitempty"`

	// InstalledDistro is the distro the most recent payload landed
	// in. Switching distros forces a reinstall in the new distro
	// even when the embedded payload hasn't changed.
	InstalledDistro string `json:"installed_distro,omitempty"`

	// InstalledBinPath is the absolute Linux path the payload was
	// installed at, recorded together with InstalledSHA256. A launch whose
	// payload bytes and distro already match reuses it and skips the wsl.exe
	// round trip that resolves $HOME (measured at ~440 ms per boot). A
	// stale path (the distro's default user or HOME changed) fails the
	// launch, and the launcher then re-resolves and reinstalls once.
	InstalledBinPath string `json:"installed_bin_path,omitempty"`
}

// HasPayload reports whether this record identifies the exact embedded build.
// Semantic versions are intentionally absent from the decision.
func (c *Config) HasPayload(distro, fingerprint string) bool {
	return c != nil && len(fingerprint) == 64 && c.InstalledSHA256 == fingerprint && c.InstalledDistro == distro
}

// InvalidatePayload must complete before replacing installed bytes. A failed
// install or boot then cannot leave the previous build's cache entry over a
// different executable. Distro selection is independent and stays unchanged.
func InvalidatePayload(dir string) error {
	cfg, err := Load(dir)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.InstalledSHA256 == "" {
		return nil
	}
	cfg.InstalledSHA256 = ""
	return Save(dir, cfg)
}

// Load reads and decodes wsl.json from dir. Returns (nil, nil) when
// the file doesn't exist — first launch / no preference yet.
func Load(dir string) (*Config, error) {
	if dir == "" {
		return nil, errors.New("wsldistro: empty directory path")
	}
	var c Config
	found, err := atomicfile.ReadJSON(filepath.Join(dir, FileName), &c)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &c, nil
}

// Save writes the encoded wsl.json under dir, creating dir when
// missing. Returns an error rather than silently no-op'ing on a nil
// Config so a typo at the call site doesn't quietly clobber the file
// with a zero-value config.
//
// Write is atomic via tempfile + Rename (see internal/atomicfile) so a
// crash between truncate and flush can't leave a partial wsl.json that
// the next launch fails to decode. The launcher and the WSL backend
// both write this file (the launcher after a successful boot, the
// backend on a Settings change), so torn-write protection is
// load-bearing.
func Save(dir string, c *Config) error {
	if dir == "" {
		return errors.New("wsldistro: empty directory path")
	}
	if c == nil {
		return errors.New("wsldistro: nil config")
	}
	return atomicfile.WriteJSON(filepath.Join(dir, FileName), c)
}
