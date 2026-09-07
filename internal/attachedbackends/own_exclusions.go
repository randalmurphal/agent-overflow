package attachedbackends

import (
	"context"
	"path/filepath"
	"slices"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
)

// Forgetting a connection is local preference, distinct from removing a group
// member. Keep it durable so introductions don't restore a deliberately hidden
// connection after this process restarts. Explicit pairing clears it.
func (m *Manager) ownDeviceExcluded(id string) (bool, error) {
	var excluded map[string]bool
	_, err := atomicfile.ReadJSON(filepath.Join(m.dir, "excluded-own-devices.json"), &excluded)
	return excluded[id], err
}

func (m *Manager) excludeOwnDevice(id string, excluded bool) error {
	unlock := m.profiles.Lock("own-device-exclusions")
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return deviceclient.WithProfileLock(ctx, m.dir, "own-device-exclusions", func() error {
		entries := map[string]bool{}
		_, err := atomicfile.ReadJSON(filepath.Join(m.dir, "excluded-own-devices.json"), &entries)
		if err != nil {
			return err
		}
		if entries[id] == excluded {
			return nil
		}
		if entries == nil {
			entries = map[string]bool{}
		}
		if excluded {
			entries[id] = true
		} else {
			delete(entries, id)
		}
		return atomicfile.WriteJSON(filepath.Join(m.dir, "excluded-own-devices.json"), entries)
	})
}

// ExcludedOwnDeviceIDs advertises local declines so peers don't continually
// mint invitations that this installation would correctly refuse to adopt.
func (m *Manager) ExcludedOwnDeviceIDs() ([]string, error) {
	var entries map[string]bool
	_, err := atomicfile.ReadJSON(filepath.Join(m.dir, "excluded-own-devices.json"), &entries)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for id, excluded := range entries {
		if excluded {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}
