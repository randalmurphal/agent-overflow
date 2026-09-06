package appidentity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/atomicfile"
)

// DeviceName owns display metadata, never a device's keys or stable identity.
// Reads stay fresh across the host and frontend-only processes sharing a root.
type DeviceName struct{ path string }

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
	var value struct {
		Name string `json:"name"`
	}
	found, err := atomicfile.ReadJSON(n.path, &value)
	if err != nil {
		return "", fmt.Errorf("read device name: %w", err)
	}
	if !found {
		return HostDisplayName(), nil
	}
	name, err := NormalizeDeviceName(value.Name)
	if err != nil {
		return "", err
	}
	if name == "" {
		return HostDisplayName(), nil
	}
	return name, nil
}

func (n *DeviceName) Set(name string) error {
	normalized, err := NormalizeDeviceName(name)
	if err != nil {
		return err
	}
	if n == nil || n.path == "" {
		return errors.New("This installation has no configuration directory.")
	}
	return atomicfile.WriteJSON(n.path, struct {
		Name string `json:"name"`
	}{normalized})
}

// Path is the single file a process watches for external name changes.
func (n *DeviceName) Path() string {
	if n == nil {
		return ""
	}
	return n.path
}
