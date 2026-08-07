package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/atomicfile"
)

// Marker records an install the user launched. The launcher's swap happens after
// this process is gone, so nothing here can observe whether it worked: the next
// backend boot compares its own running version against ExpectedVersion and, on
// a mismatch, tells the user the swap never applied. PriorVersion is what was
// running when the install was staged, so a boot that finds it unchanged can say
// so precisely instead of guessing.
type Marker struct {
	ExpectedVersion string    `json:"expectedVersion"`
	PriorVersion    string    `json:"priorVersion"`
	StagedAt        time.Time `json:"stagedAt"`
}

// MarkerPath is where SaveMarker / LoadMarker / ClearMarker keep the marker for
// a given data dir.
func MarkerPath(dir string) string { return filepath.Join(dir, MarkerFileName) }

// SaveMarker atomically writes m into dir.
func SaveMarker(dir string, m Marker) error {
	if dir == "" {
		return errors.New("selfupdate: marker dir is empty")
	}
	if err := validateVersion(m.ExpectedVersion); err != nil {
		return fmt.Errorf("selfupdate: marker expectedVersion: %w", err)
	}
	if err := atomicfile.WriteJSON(MarkerPath(dir), m); err != nil {
		return fmt.Errorf("selfupdate: save update marker: %w", err)
	}
	return nil
}

// LoadMarker reads the marker in dir. It returns (nil, nil) when no install is
// pending — the normal boot — and an error when a marker exists but cannot be
// read or decoded, so a corrupt marker is never silently treated as "none".
func LoadMarker(dir string) (*Marker, error) {
	if dir == "" {
		return nil, errors.New("selfupdate: marker dir is empty")
	}
	var m Marker
	found, err := atomicfile.ReadJSON(MarkerPath(dir), &m)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: load update marker: %w", err)
	}
	if !found {
		return nil, nil
	}
	return &m, nil
}

// ClearMarker removes the marker. It is idempotent: an already-absent marker is
// success, so the boot path can clear unconditionally.
func ClearMarker(dir string) error {
	if dir == "" {
		return errors.New("selfupdate: marker dir is empty")
	}
	if err := os.Remove(MarkerPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("selfupdate: clear update marker: %w", err)
	}
	return nil
}
