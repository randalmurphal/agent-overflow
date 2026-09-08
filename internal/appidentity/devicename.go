package appidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/atomicfile"
)

// DeviceName owns display metadata, never a device's keys or stable identity.
// Reads stay fresh across the host and frontend-only processes sharing a root:
// each Get stats the file and re-reads it only when its size or mtime moved,
// so the getters that run per LAN query cost a stat, not a parse.
type DeviceName struct {
	path  string
	mu    sync.Mutex
	stamp string // size@mtime of the file the cached name was read from
	name  string // the file's normalized name; empty defers to the host name
}

func NewDeviceName(configDir string) *DeviceName {
	if configDir == "" {
		return &DeviceName{}
	}
	return &DeviceName{path: filepath.Join(configDir, "device-name.json")}
}

func NormalizeDeviceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 {
		return "", errors.New("Device name must contain at most 80 valid characters.")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("Device name cannot contain control characters.")
		}
	}
	return name, nil
}

func (n *DeviceName) Get() (string, error) {
	if n == nil || n.path == "" {
		return HostDisplayName(), nil
	}
	stamp := "absent"
	if info, err := os.Stat(n.path); err == nil {
		stamp = fmt.Sprintf("%d@%d", info.Size(), info.ModTime().UnixNano())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if stamp != n.stamp {
		name, err := n.read()
		if err != nil {
			return "", err
		}
		n.stamp, n.name = stamp, name
	}
	if n.name == "" {
		return HostDisplayName(), nil
	}
	return n.name, nil
}

func (n *DeviceName) read() (string, error) {
	var value struct {
		Name string `json:"name"`
	}
	found, err := atomicfile.ReadJSON(n.path, &value)
	if err != nil {
		return "", fmt.Errorf("read device name: %w", err)
	}
	if !found {
		return "", nil
	}
	return NormalizeDeviceName(value.Name)
}

func (n *DeviceName) Set(name string) error {
	normalized, err := NormalizeDeviceName(name)
	if err != nil {
		return err
	}
	if n == nil || n.path == "" {
		return errors.New("This installation has no configuration directory.")
	}
	if err := atomicfile.WriteJSON(n.path, struct {
		Name string `json:"name"`
	}{normalized}); err != nil {
		return err
	}
	// The next Get re-reads whatever the stamp says: a rewrite inside one
	// mtime tick with the same length is invisible to the stat alone.
	n.mu.Lock()
	n.stamp = ""
	n.mu.Unlock()
	return nil
}

// Path is the single file a process watches for external name changes.
func (n *DeviceName) Path() string {
	if n == nil {
		return ""
	}
	return n.path
}
