package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
)

// FrontendOwnDeviceHooks keeps only public membership metadata for a desktop
// controller with no execution database. Its device key and direct sessions are
// still the installation's existing deviceclient profile, shared with --connect.
func (m *Manager) FrontendOwnDeviceHooks(changed func()) OwnDeviceHooks {
	return OwnDeviceHooks{Snapshot: m.frontendOwnSnapshot, Accept: m.acceptFrontendOwnSource, Changed: changed}
}

func (m *Manager) frontendOwnSnapshot() (owndevices.List, error) {
	out := owndevices.List{Members: []owndevices.Member{}}
	key, err := m.OwnIdentity()
	if errors.Is(err, deviceclient.ErrNoDeviceKey) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.SelfKeyThumbprint = key
	_, err = atomicfile.ReadJSON(filepath.Join(m.dir, "own-devices.json"), &out.Members)
	if err != nil {
		return out, err
	}
	self, ok := ownMember(out.Members, key)
	out.Enabled = ok && !self.Removed
	out.ConnectedBackendIDs, err = m.ConnectedOwnDeviceIDs()
	if err != nil {
		return out, err
	}
	out.ExcludedBackendIDs, err = m.ExcludedOwnDeviceIDs()
	if err != nil {
		return out, err
	}
	return out, nil
}

func (m *Manager) acceptFrontendOwnSource(source owndevices.List) (bool, error) {
	if !source.Enabled {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changed := false
	err := deviceclient.WithProfileLock(ctx, m.dir, "own-device-catalog", func() error {
		current, err := m.frontendOwnSnapshot()
		if err != nil {
			return err
		}
		members, merged, err := owndevices.Merge(current.Members, source.Members)
		if err != nil {
			return err
		}
		_, ok := ownMember(members, current.SelfKeyThumbprint)
		if !ok {
			return errors.New("this frontend is not an active own device")
		}
		// A directly authenticated source can refresh its own metadata, but
		// cannot rewrite another machine's previously established TLS trust.
		for i, member := range members {
			if member.KeyThumbprint == source.SelfKeyThumbprint && !member.Removed {
				fresh, _ := ownMember(source.Members, member.KeyThumbprint)
				if member.BackendID != "" && member.BackendID != fresh.BackendID {
					return errors.New("own-device backend identity changed")
				}
				fresh.Generation, fresh.Removed = member.Generation, member.Removed
				members[i] = fresh
			}
		}
		// Retain a backend descriptor learned from this installation's host:
		// frontend-only mode shares its key and must not erase that identity.
		name, err := m.localLabel()
		if err != nil {
			return err
		}
		for i := range members {
			if members[i].KeyThumbprint == current.SelfKeyThumbprint {
				members[i].Name = name
			}
		}
		_, _, err = owndevices.Merge(members, nil)
		if err != nil {
			return err
		}
		changed = merged || !reflect.DeepEqual(current.Members, members)
		if !changed {
			return nil
		}
		return atomicfile.WriteJSON(filepath.Join(m.dir, "own-devices.json"), members)
	})
	return changed, err
}
