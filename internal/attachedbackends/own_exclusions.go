package attachedbackends

import (
	"path/filepath"
	"slices"

	"agent-overflow/internal/atomicfile"
)

// Forgetting a connection is local preference, distinct from removing a group
// member. Keep it durable so introductions don't restore a deliberately hidden
// connection after this process restarts. Explicit pairing clears it.
func (m *Manager) ownDeviceExcluded(id string) (bool, error) {
	var excluded map[string]bool
	_, err := atomicfile.ReadJSON(filepath.Join(m.dir, ownExclusionsFile), &excluded)
	return excluded[id], err
}

// ExcludedOwnDeviceIDs advertises local declines so peers don't continually
// mint invitations that this installation would correctly refuse to adopt.
func (m *Manager) ExcludedOwnDeviceIDs() ([]string, error) {
	var entries map[string]bool
	_, err := atomicfile.ReadJSON(filepath.Join(m.dir, ownExclusionsFile), &entries)
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
