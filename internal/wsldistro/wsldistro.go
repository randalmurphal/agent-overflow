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
// the launcher (which writes Distro / InstalledVer / InstalledDistro
// after a successful boot) and the WSL backend (which mutates only
// Distro to honor a Settings UI distro switch — InstalledVer and
// InstalledDistro are owned by the launcher's install path).
type Config struct {
	// Distro is the WSL distribution name the launcher should boot
	// into on the next launch.
	Distro string `json:"distro"`

	// InstalledVer is the payloadVersion stamped onto the launcher
	// at build time, recorded after a successful install + boot. The
	// launcher uses this to decide whether to reinstall the Linux
	// payload on the next launch.
	InstalledVer string `json:"installed_version,omitempty"`

	// InstalledDistro is the distro the most recent payload landed
	// in. Switching distros forces a reinstall in the new distro
	// even when InstalledVer hasn't changed.
	InstalledDistro string `json:"installed_distro,omitempty"`

	// InstalledBinPath is the absolute Linux path the payload was
	// installed at, recorded together with InstalledVer. A launch whose
	// version and distro already match reuses it and skips the wsl.exe
	// round trip that resolves $HOME (measured at ~440 ms per boot). A
	// stale path (the distro's default user or HOME changed) fails the
	// launch, and the launcher then re-resolves and reinstalls once.
	InstalledBinPath string `json:"installed_bin_path,omitempty"`
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
