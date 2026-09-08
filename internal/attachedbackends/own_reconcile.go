package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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
	// after schedules the retry that follows a failed pass. Nil is
	// time.After; a test replaces it to watch what gets scheduled.
	after func(time.Duration) <-chan time.Time
}

// A pass in which some peer failed is retried, doubling from ownRetryMin
// to ownRetryMax until a pass succeeds or a wake resets it. A pass in which
// every peer answered schedules nothing at all: propagation is push-driven
// (NotifyOwnDevices wakes this loop), so a healthy set costs no traffic —
// each pass per peer is a ticket mint, a socket dial, a hello and several
// RPCs, which is not a price to pay every thirty seconds for no change.
const (
	ownRetryMin = 30 * time.Second
	ownRetryMax = 5 * time.Minute
)

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

// StartOwnDevices reconciles at boot and on every wake, and retries with
// backoff after a pass some peer failed. Offline members retry without a
// frontend or a permanently running hub. At most four bounded peer
// exchanges run together, so an unavailable host cannot block healthy ones.
func (m *Manager) StartOwnDevices(ctx context.Context, hooks OwnDeviceHooks) {
	m.own.startOnce.Do(func() {
		after := m.own.after
		if after == nil {
			after = time.After
		}
		m.own.wg.Go(func() {
			var backoff time.Duration
			for ctx.Err() == nil {
				var retry <-chan time.Time
				if m.ReconcileOwnDevices(ctx, hooks) {
					backoff = min(max(2*backoff, ownRetryMin), ownRetryMax)
					retry = after(backoff)
				} else {
					backoff = 0
				}
				select {
				case <-ctx.Done():
				case <-m.ownWake():
					backoff = 0
				case <-retry:
				}
			}
		})
	})
}

func (m *Manager) WaitOwnDevices() { m.own.wg.Wait() }

// ReconcileOwnDevices executes one pass and reports whether any peer failed.
// Per-peer errors are retained on its existing carrier for the settings
// surface; outages never delete pairings.
func (m *Manager) ReconcileOwnDevices(ctx context.Context, hooks OwnDeviceHooks) (failed bool) {
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
		return true
	}
	if len(profiles) > owndevices.MaxMembers {
		profiles = profiles[:owndevices.MaxMembers]
	}
	var failures atomic.Int32
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
				message := ""
				if err != nil && ctx.Err() == nil {
					message = err.Error()
					failures.Add(1)
				}
				if held, getErr := m.carrier(id); getErr == nil {
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
	return stateErr != nil || failures.Load() > 0
}

func (m *Manager) reconcileOwnPeer(ctx context.Context, id string, hooks OwnDeviceHooks) error {
	held, err := m.carrier(id)
	if err != nil {
		return err
	}
	rpc, err := held.openRPC(ctx, owndevices.Capability)
	if err != nil {
		if held.client.Retired() {
			// The far side ended this session: openRPC retired the row,
			// and its profile is gone. Not an outage to retry.
			return nil
		}
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
	if err = validateOwnSource(remote, id, key); errors.Is(err, errOwnDeviceRemoved) {
		return m.removedBy(id, remote, hooks)
	} else if err != nil {
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
	if err = validateOwnSource(remote, id, key); errors.Is(err, errOwnDeviceRemoved) {
		return m.removedBy(id, remote, hooks)
	} else if err != nil {
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

// errOwnDeviceRemoved is a catalog in which the caller itself is a tombstone:
// the far side's notice that this device was removed from the group.
var errOwnDeviceRemoved = errors.New("this device was removed from that computer's group")

func validateOwnSource(source owndevices.List, backendID, key string) error {
	if !source.Enabled || len(source.Members) > owndevices.MaxMembers {
		return errors.New("invalid own-device membership response")
	}
	self, ok := ownMember(source.Members, source.SelfKeyThumbprint)
	if !ok || self.Removed || self.BackendID != backendID {
		return errors.New("own-device membership does not match the paired computer")
	}
	caller, ok := ownMember(source.Members, key)
	if !ok {
		return errors.New("this device is not a member of that computer's group")
	}
	if caller.Removed {
		return errOwnDeviceRemoved
	}
	return nil
}

// removedBy absorbs the far side's tombstone for this device. The catalog is
// accepted like any other — removal wins the merge, and pruning then retires
// every own-device profile — and should a stale local generation keep this
// device active, that peer's own-device profile still retires: rejoining
// takes a fresh approval on the far side, never a retry from here.
func (m *Manager) removedBy(id string, remote owndevices.List, hooks OwnDeviceHooks) error {
	if _, err := hooks.Accept(remote); err != nil {
		return err
	}
	local, err := hooks.Snapshot()
	if err != nil {
		return err
	}
	pruned, err := m.pruneOwnProfiles(local)
	if err != nil {
		return err
	}
	dropped, err := m.retireOwnProfile(id, "")
	if err != nil {
		return err
	}
	if (pruned || dropped) && hooks.Changed != nil {
		hooks.Changed()
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
		dropped, err := m.retireOwnProfile(session.BackendID, session.SessionID)
		changed = changed || dropped
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// retireOwnProfile forgets one own-device profile the way a tombstone does,
// if it still holds the session named (any session when sessionID is empty).
// Ordinary limited shares are separate grants and are never touched.
func (m *Manager) retireOwnProfile(id, sessionID string) (bool, error) {
	unlock := m.profiles.Lock(id)
	defer unlock()
	current, err := deviceclient.LoadSession(m.dir, id)
	if err != nil {
		if errors.Is(err, deviceclient.ErrNoSession) {
			err = nil
		}
		return false, err
	}
	if !current.OwnDevice || (sessionID != "" && current.SessionID != sessionID) {
		return false, nil
	}
	if err := m.forgetLocked(id); err != nil {
		return false, err
	}
	return true, nil
}
