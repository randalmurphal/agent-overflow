package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
)

// OwnDeviceHooks keep durable membership authority with the host identity
// service (or the frontend-only public catalog). The manager owns only direct
// paired connections and a bounded retry worker, never a second group model.
type OwnDeviceHooks struct {
	Snapshot func() (owndevices.List, error)
	Accept   func(owndevices.List) (bool, error)
	Mint     func(context.Context, string) (string, error)
	Changed  func()
}

type ownDeviceReconciler struct {
	wakeOnce  sync.Once
	startOnce sync.Once
	wake      chan struct{}
	wg        sync.WaitGroup
}

func (m *Manager) ownWake() chan struct{} {
	m.own.wakeOnce.Do(func() { m.own.wake = make(chan struct{}, 1) })
	return m.own.wake
}

func (m *Manager) WakeOwnDevices() {
	select {
	case m.ownWake() <- struct{}{}:
	default:
	}
}

// StartOwnDevices reconciles at boot and on changes. Offline members retry
// without a frontend or a permanently running hub. At most four bounded peer
// exchanges run together, so an unavailable host cannot block healthy ones.
func (m *Manager) StartOwnDevices(ctx context.Context, hooks OwnDeviceHooks) {
	m.own.startOnce.Do(func() {
		m.own.wg.Go(func() {
			for ctx.Err() == nil {
				m.ReconcileOwnDevices(ctx, hooks)
				var timer *time.Timer
				var tick <-chan time.Time
				if connected, _ := m.ConnectedOwnDeviceIDs(); len(connected) > 0 {
					timer = time.NewTimer(30 * time.Second)
					tick = timer.C
				}
				select {
				case <-ctx.Done():
				case <-m.ownWake():
				case <-tick:
				}
				if timer != nil {
					timer.Stop()
				}
			}
		})
	})
}

func (m *Manager) WaitOwnDevices() { m.own.wg.Wait() }

// ReconcileOwnDevices executes one pass. Per-peer errors are retained on its
// existing carrier for the settings surface; outages never delete pairings.
func (m *Manager) ReconcileOwnDevices(ctx context.Context, hooks OwnDeviceHooks) {
	snapshot, stateErr := hooks.Snapshot()
	if stateErr == nil {
		var changed bool
		changed, stateErr = m.pruneOwnProfiles(snapshot)
		if changed && hooks.Changed != nil {
			hooks.Changed()
		}
	}
	profiles, err := m.ConnectedOwnDeviceIDs()
	if err != nil {
		return
	}
	if len(profiles) > owndevices.MaxMembers {
		profiles = profiles[:owndevices.MaxMembers]
	}
	jobs := make(chan string)
	var workers sync.WaitGroup
	for range min(4, len(profiles)) {
		workers.Go(func() {
			for id := range jobs {
				peerCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				err := stateErr
				if err == nil {
					err = m.reconcileOwnPeer(peerCtx, id, hooks)
				}
				cancel()
				if held, getErr := m.carrier(id); getErr == nil {
					message := ""
					if err != nil && ctx.Err() == nil {
						message = err.Error()
					}
					if held.setOwnError(message) && hooks.Changed != nil {
						hooks.Changed()
					}
				}
			}
		})
	}
	for _, profile := range profiles {
		select {
		case jobs <- profile:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
}

func (m *Manager) reconcileOwnPeer(ctx context.Context, id string, hooks OwnDeviceHooks) error {
	held, err := m.carrier(id)
	if err != nil {
		return err
	}
	rpc, err := held.openRPC(ctx, owndevices.Capability)
	if err != nil {
		return err
	}
	defer rpc.Close()
	var remote owndevices.List
	if err = rpc.Call(ctx, "ListOwnDevices", &remote); err != nil {
		return err
	}
	if !remote.Enabled {
		return nil
	} // Full-access legacy/session sharing is not membership.
	key, err := m.OwnIdentity()
	if err != nil {
		return err
	}
	if err = validateOwnSource(remote, id, key); err != nil {
		return err
	}
	if _, err = hooks.Accept(remote); err != nil {
		return err
	}
	local, err := hooks.Snapshot()
	if err != nil {
		return err
	}
	if changed, err := m.pruneOwnProfiles(local); err != nil {
		return err
	} else if changed && hooks.Changed != nil {
		hooks.Changed()
	}
	self, ok := ownMember(local.Members, key)
	if !local.Enabled || !ok || self.Removed {
		return nil // A local removal wins over a stale, still-online member.
	}
	if err = rpc.Call(ctx, "RegisterOwnDevice", &remote, self); err != nil {
		return err
	}
	if err = rpc.Call(ctx, "SyncOwnDevices", &remote, local.Members); err != nil {
		return err
	}
	if err = validateOwnSource(remote, id, key); err != nil {
		return err
	}
	if _, err = hooks.Accept(remote); err != nil {
		return err
	}
	if hooks.Mint != nil && self.BackendID != "" && !slices.Contains(remote.ConnectedBackendIDs, self.BackendID) && !slices.Contains(remote.ExcludedBackendIDs, self.BackendID) {
		link, mintErr := hooks.Mint(ctx, remote.SelfKeyThumbprint)
		if mintErr != nil {
			return mintErr
		}
		if err = rpc.Call(ctx, "AcceptOwnDeviceIntroduction", nil, link); err != nil {
			return err
		}
	}
	var introductionErrors []error
	for _, member := range remote.Members {
		if member.Removed || member.BackendID == "" || member.KeyThumbprint == key || member.BackendID == id {
			continue
		}
		if excluded, err := m.ownDeviceExcluded(member.BackendID); err != nil {
			return err
		} else if excluded {
			continue
		}
		if session, err := deviceclient.LoadSession(m.dir, member.BackendID); err == nil && session.OwnDevice {
			continue
		} else if err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
			return err
		}
		var invitation struct {
			URL string `json:"url"`
		}
		if err := rpc.Call(ctx, "IntroduceOwnDevice", &invitation, member.BackendID); err != nil {
			introductionErrors = append(introductionErrors, fmt.Errorf("connect computer %s: %w", member.BackendID, err))
			continue
		}
		link, err := deviceclient.DecodeLink(invitation.URL)
		if err != nil || link.BackendID != member.BackendID {
			introductionErrors = append(introductionErrors, fmt.Errorf("connect computer %s: invalid own-device invitation", member.BackendID))
			continue
		}
		added, err := m.AcceptOwnDeviceIntroduction(ctx, invitation.URL, member.Routes)
		if err != nil {
			introductionErrors = append(introductionErrors, fmt.Errorf("connect computer %s: %w", member.BackendID, err))
			continue
		}
		if added && hooks.Changed != nil {
			hooks.Changed()
		}
	}
	return errors.Join(introductionErrors...)
}

func validateOwnSource(source owndevices.List, backendID, key string) error {
	if !source.Enabled || len(source.Members) > owndevices.MaxMembers {
		return errors.New("invalid own-device membership response")
	}
	self, ok := ownMember(source.Members, source.SelfKeyThumbprint)
	if !ok || self.Removed || self.BackendID != backendID {
		return errors.New("own-device membership does not match the paired computer")
	}
	caller, ok := ownMember(source.Members, key)
	if !ok || caller.Removed {
		return errors.New("this device is not a member of that computer's group")
	}
	return nil
}
func ownMember(members []owndevices.Member, key string) (owndevices.Member, bool) {
	for _, member := range members {
		if member.KeyThumbprint == key {
			return member, true
		}
	}
	return owndevices.Member{}, false
}

func (c *carrier) setOwnError(message string) bool {
	c.nameErrorMu.Lock()
	defer c.nameErrorMu.Unlock()
	changed := c.ownSyncError != message
	c.ownSyncError = message
	return changed
}
func (c *carrier) ownError() string {
	c.nameErrorMu.Lock()
	defer c.nameErrorMu.Unlock()
	return c.ownSyncError
}

var errUnsupportedPeerOperation = errors.New("paired computer needs an update to support this operation")

// Tombstones remove only profiles created by own-device enrollment. Ordinary
// limited shares are separate grants and are never absorbed into group policy.
func (m *Manager) pruneOwnProfiles(snapshot owndevices.List) (bool, error) {
	self, known := ownMember(snapshot.Members, snapshot.SelfKeyThumbprint)
	removed := make(map[string]bool)
	for _, member := range snapshot.Members {
		if member.Removed && member.BackendID != "" {
			removed[member.BackendID] = true
		}
	}
	sessions, err := deviceclient.ListSessions(m.dir)
	if err != nil {
		return false, err
	}
	changed := false
	for _, session := range sessions {
		if !session.OwnDevice || !(removed[session.BackendID] || (known && self.Removed)) {
			continue
		}
		unlock := m.profiles.Lock(session.BackendID)
		current, err := deviceclient.LoadSession(m.dir, session.BackendID)
		if err == nil && current.OwnDevice && current.SessionID == session.SessionID {
			err = m.forgetLocked(session.BackendID)
			changed = changed || err == nil
		}
		unlock()
		if err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
			return changed, err
		}
	}
	return changed, nil
}
