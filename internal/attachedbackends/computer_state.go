package attachedbackends

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
)

// A computer's local pairing state is its session file, its entry in the
// own-device exclusions (excluded-own-devices.json) and in agent access
// (agent-access.json), and the carrier this process caches for it. Two
// managers can share one profile directory (the host and the frontend-only
// desktop), so every change to that state is one transition run by
// transact, and no other code writes any of it. The one exception is the
// lookup (carrier), which under the profile lock fills an empty cache from
// the stored session and evicts a retired carrier, whose client never
// writes again; it changes no pairing. The session file is
// written through the transaction's held session, and the two shared files
// only by computerTx's methods, which nothing outside a transaction can
// call. TestOnlyTheComputerTransactionWritesItsState is the backstop.
//
// Lock order, for every transition:
//
//  1. the computer's profile lock (Manager.profiles, in-process);
//  2. its session file's OS lock (deviceclient.WithSession), the
//     cross-process order;
//  3. at most one shared file's locks at a time, briefly: its in-process
//     key in Manager.sharedFiles, then its OS lock (deviceclient.WithProfileLock).
//     Not Manager.profiles: a computer id may spell a shared file's lock
//     name, and its transition would then wait on its own profile lock;
//  4. Manager.mu, a leaf: nothing waits while holding it
//     (TestNothingWaitsUnderTheManagerMutex).
//
// A client's mutex is only ever taken before a session file's lock
// (deviceclient's sessionTransaction), so a transition takes none: carriers
// it replaces or drops are retired after the session file is released.
//
// Writes within a transition are ordered so that a crash between two
// leaves the safe side, and a failed step is rolled back inside the
// transaction to the values it read there, which no other writer can have
// changed meanwhile.

// computerTx is one computer's local pairing state held for one transition.
type computerTx struct {
	m    *Manager
	id   string
	held *deviceclient.HeldSession
	// leaving are the carriers this transition took out of the cache, each
	// with the verdict its connections close with.
	leaving []leavingCarrier
}

type leavingCarrier struct {
	carrier *carrier
	verdict error
}

// transact runs transition on id's local pairing state (computerTx).
func (m *Manager) transact(id string, transition func(*computerTx) error) error {
	unlock := m.profiles.Lock(id)
	defer unlock()
	tx := &computerTx{m: m, id: id}
	err := deviceclient.WithSession(m.dir, id, func(held *deviceclient.HeldSession) error {
		tx.held = held
		return transition(tx)
	})
	// Still under the profile lock, so no lookup builds another owner from
	// the file meanwhile.
	for _, gone := range tx.leaving {
		gone.carrier.leave(gone.verdict)
		gone.carrier.client.Retire()
	}
	return err
}

// stored is the session in the computer's file now.
func (tx *computerTx) stored() (deviceclient.Session, error) { return tx.held.Stored() }

// restore writes back a session read earlier in this transaction, or drops
// the file if none was stored then.
func (tx *computerTx) restore(stored deviceclient.Session, loadErr error) error {
	if errors.Is(loadErr, deviceclient.ErrNoSession) {
		return tx.held.Remove()
	}
	if loadErr != nil {
		// A file that could not be read is never overwritten by a rollback.
		return nil
	}
	return tx.held.Write(stored)
}

// cached is the carrier this process caches for the computer.
func (tx *computerTx) cached() *carrier {
	tx.m.mu.Lock()
	defer tx.m.mu.Unlock()
	return tx.m.carriers[tx.id]
}

// install caches and renews built in place of whatever carrier was cached,
// which leaves once the session file is released. After Close nothing is
// cached or renewed and built is not installed.
func (tx *computerTx) install(built *carrier) bool {
	tx.m.mu.Lock()
	defer tx.m.mu.Unlock()
	if old := tx.m.carriers[tx.id]; old != nil {
		tx.leaving = append(tx.leaving, leavingCarrier{old, errPairingReplaced})
		delete(tx.m.carriers, tx.id)
	}
	delete(tx.m.verdicts, tx.id)
	if tx.m.closed {
		return false
	}
	tx.m.wire(built, tx.id)
	tx.m.carriers[tx.id] = built
	return true
}

// drop takes the cached carrier out of the cache; it leaves with verdict
// once the session file is released.
func (tx *computerTx) drop(verdict error) {
	tx.m.mu.Lock()
	defer tx.m.mu.Unlock()
	if old := tx.m.carriers[tx.id]; old != nil {
		tx.leaving = append(tx.leaving, leavingCarrier{old, verdict})
		delete(tx.m.carriers, tx.id)
	}
}

// excluded reports whether the computer is excluded as an own device.
func (tx *computerTx) excluded() (bool, error) { return tx.m.ownDeviceExcluded(tx.id) }

// setExcluded records or clears the computer's own-device exclusion. It is
// the one writer of excluded-own-devices.json.
func (tx *computerTx) setExcluded(excluded bool) error {
	unlock := tx.m.sharedFiles.Lock(ownExclusionsLock)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return deviceclient.WithProfileLock(ctx, tx.m.dir, ownExclusionsLock, func() error {
		entries := map[string]bool{}
		if _, err := atomicfile.ReadJSON(filepath.Join(tx.m.dir, ownExclusionsFile), &entries); err != nil {
			return err
		}
		if entries[tx.id] == excluded {
			return nil
		}
		if entries == nil {
			entries = map[string]bool{}
		}
		if excluded {
			entries[tx.id] = true
		} else {
			delete(entries, tx.id)
		}
		return atomicfile.WriteJSON(filepath.Join(tx.m.dir, ownExclusionsFile), entries)
	})
}

// agentAccess reports whether the computer's agent opt-in is on.
func (tx *computerTx) agentAccess() (bool, error) {
	access, err := tx.m.AgentAccess()
	return access[tx.id], err
}

// setAgentAccess turns the computer's agent opt-in on or off. It is the one
// writer of agent-access.json. Turning one on also prunes the opt-ins of
// computers with no session stored, which only a verdict leaves behind.
func (tx *computerTx) setAgentAccess(enabled bool) error {
	unlock := tx.m.sharedFiles.Lock(agentAccessLock)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return deviceclient.WithProfileLock(ctx, tx.m.dir, agentAccessLock, func() error {
		access, err := tx.m.AgentAccess()
		if err != nil {
			return err
		}
		if enabled {
			for peer := range access {
				if peer == tx.id {
					continue
				}
				if _, err := deviceclient.LoadSession(tx.m.dir, peer); errors.Is(err, deviceclient.ErrNoSession) {
					delete(access, peer)
				}
			}
			if !access[tx.id] && len(access) >= MaxAgentComputers {
				return errors.New("at most 16 computers can be enabled for agent commands")
			}
			access[tx.id] = true
		} else {
			if !access[tx.id] {
				return nil
			}
			delete(access, tx.id)
		}
		return atomicfile.WriteJSON(filepath.Join(tx.m.dir, agentAccessFile), access)
	})
}

// The shared files and the names of their locks.
const (
	ownExclusionsFile = "excluded-own-devices.json"
	ownExclusionsLock = "own-device-exclusions"
	agentAccessFile   = "agent-access.json"
	agentAccessLock   = "agent-access"
)

// commitEnrollment saves an answered redemption, unless the enrollment was
// cancelled or the computer is not wanted any more (admitEnrollment), both
// decided in the transaction, so neither a forget nor a pairing another
// manager wrote since the check before the redemption is overwritten. The
// carrier is swapped only once every write has succeeded (savePairing):
// until then, and after a refusal or a failure, the cached carrier and its
// connections stay as they were. The swap happens under the session file's
// lock, which is what fences the old carrier's verdict path: a rotation it
// has in flight sees the replacement only after the lock is released, by
// when it is no longer the cached carrier (endSession).
func (m *Manager) commitEnrollment(e *enrollment, link deviceclient.Link, redeemed *deviceclient.Redemption) (Attachment, *carrier, error) {
	var attachment Attachment
	var installed *carrier
	err := m.transact(link.BackendID, func(tx *computerTx) error {
		if !m.moveEnrollment(e, enrollmentRedeeming, enrollmentCommitted) {
			return context.Cause(e.ctx)
		}
		stored, loadErr := tx.stored()
		if err := m.admitEnrollment(link.BackendID, e, stored, loadErr); err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return err
		}
		built, pairing, err := m.savePairing(tx, e.purpose == introductionByPeer, redeemed, stored, loadErr)
		if err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return err
		}
		if tx.install(built) {
			installed = built
		}
		attachment = Attachment{
			ID:                 link.BackendID,
			Name:               displayName(built.client.Session()),
			Endpoint:           link.Endpoint,
			VerificationNumber: pairing.VerificationNumber,
		}
		return nil
	})
	return attachment, installed, err
}

// savePairing makes an enrollment's writes and builds its carrier, in
// order, and if any step fails rolls back every write it made to the values
// read in this transaction (stored, loadErr and its own reads), so a failed
// commit leaves the session, the exclusion and agent access as they were.
//
// A person's pairing is a new grant. Its steps: agent access off, the
// session (no nickname), the carrier built, the exclusion cleared. A crash
// after the session leaves the computer paired and still excluded, so no
// automatic introduction follows. An introduction writes only the session,
// keeping agent access and the stored nickname: it upgrades a pairing, it
// does not grant one.
func (m *Manager) savePairing(tx *computerTx, introduction bool, redeemed *deviceclient.Redemption, stored deviceclient.Session, loadErr error) (built *carrier, pairing deviceclient.Pairing, err error) {
	var undo []func() error
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				err = errors.Join(err, undo[i]())
			}
			built, pairing = nil, deviceclient.Pairing{}
		}
	}()
	nickname, wasExcluded := "", false
	if introduction {
		if loadErr == nil {
			nickname = stored.Nickname
		}
	} else {
		granted, err := tx.agentAccess()
		if err != nil {
			return nil, deviceclient.Pairing{}, err
		}
		if wasExcluded, err = tx.excluded(); err != nil {
			return nil, deviceclient.Pairing{}, err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return nil, deviceclient.Pairing{}, err
		}
		if granted {
			undo = append(undo, func() error { return tx.setAgentAccess(true) })
		}
	}
	client, pairing, err := redeemed.Save(tx.held, nickname)
	if err != nil {
		return nil, deviceclient.Pairing{}, err
	}
	undo = append(undo, func() error { return tx.restore(stored, loadErr) })
	if built, err = m.buildCarrier(client, tx.id); err != nil {
		return nil, deviceclient.Pairing{}, err
	}
	if wasExcluded {
		if err := tx.setExcluded(false); err != nil {
			return nil, deviceclient.Pairing{}, err
		}
	}
	return built, pairing, nil
}

// Remove forgets one machine, whichever pairing with it is stored, also one
// another manager over the same profile directory saved. The device key
// survives: it names this DEVICE, and the far side adopts its row by
// thumbprint if this installation ever pairs with it again.
//
// A pairing or introduction with the computer still in flight is cancelled
// first, without waiting for it: whatever its redemption answers is not
// saved (enrollment). Forgetting is the decision, so it stays cancelled
// even if the removal below fails.
//
// Its writes, in order: the exclusion, agent access off, the session
// deleted. A crash between them leaves the computer forgotten or still
// paired, never paired with the exclusion gone. A removal that fails gives
// the exclusion and the opt-in back, and its old owner is retired all the
// same, so the lookup that rebuilds a carrier never has two.
func (m *Manager) Remove(id string) error {
	m.cancelEnrollment(id, errPairingForgotten)
	err := m.transact(id, func(tx *computerTx) error {
		wasExcluded, err := tx.excluded()
		if err != nil {
			return err
		}
		granted, err := tx.agentAccess()
		if err != nil {
			return err
		}
		rollBack := func(err error) error {
			if granted {
				err = errors.Join(err, tx.setAgentAccess(true))
			}
			if !wasExcluded {
				err = errors.Join(err, tx.setExcluded(false))
			}
			return err
		}
		if err := tx.setExcluded(true); err != nil {
			return err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return rollBack(err)
		}
		tx.drop(errPairingForgotten)
		if err := tx.held.Remove(); err != nil {
			return rollBack(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	m.notifyChanged(SetChange{Action: SetRemoved, ID: id})
	return nil
}

// retireOwnProfile forgets one own-device profile the way a tombstone does,
// if it holds the session named (any session when sessionID is empty), and
// announces it as SetRemoved, as every removal is. Ordinary limited shares
// are separate grants and are never touched, and neither is a pairing
// another manager saved in place of the named one. Its writes, in order:
// agent access off, the session deleted.
func (m *Manager) retireOwnProfile(id, sessionID string) (bool, error) {
	dropped := false
	err := m.transact(id, func(tx *computerTx) error {
		stored, err := tx.stored()
		if errors.Is(err, deviceclient.ErrNoSession) {
			return nil
		}
		if err != nil || !stored.OwnDevice || (sessionID != "" && stored.SessionID != sessionID) {
			return err
		}
		granted, err := tx.agentAccess()
		if err != nil {
			return err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return err
		}
		tx.drop(errPairingForgotten)
		if err := tx.held.Remove(); err != nil {
			if granted {
				err = errors.Join(err, tx.setAgentAccess(true))
			}
			return err
		}
		dropped = true
		return nil
	})
	if err != nil || !dropped {
		return false, err
	}
	m.notifyChanged(SetChange{Action: SetRemoved, ID: id})
	return true, nil
}

// SetAgentAccess turns one computer's agent opt-in on or off. Turning it
// on needs a session stored for the computer.
func (m *Manager) SetAgentAccess(id string, enabled bool) error {
	if !entityid.Valid(id) {
		return errors.New("invalid computer identity")
	}
	return m.transact(id, func(tx *computerTx) error {
		if enabled {
			if _, err := tx.stored(); err != nil {
				return err
			}
		}
		return tx.setAgentAccess(enabled)
	})
}

// endSession is the verdict path: the far side stopped honouring one
// pairing for good. deviceclient has already dropped the refused session
// file and retired the owner; what is left is this process's cache and the
// people watching it. A carrier that is no longer the cached one was
// replaced, and nothing here touches it. Otherwise the carrier leaves, and
// if no pairing has been stored since, its agent opt-in goes the way a
// removal takes it, the verdict is kept with the name the pairing gave the
// computer (TakeVerdict), and the observer retires the row. A pairing
// another manager stored since keeps its grant and its row.
func (m *Manager) endSession(id string, held *carrier) {
	name := strings.Join(strings.Fields(displayName(held.client.Session())), " ")
	removed := false
	err := m.transact(id, func(tx *computerTx) error {
		if tx.cached() != held {
			return nil
		}
		tx.drop(errPairingEnded)
		if _, err := tx.stored(); !errors.Is(err, deviceclient.ErrNoSession) {
			return err
		}
		m.mu.Lock()
		m.verdicts[id] = name
		m.mu.Unlock()
		removed = true
		return tx.setAgentAccess(false)
	})
	if err != nil {
		// Nobody is waiting on a verdict; the log is where it surfaces.
		log.Printf("attachedbackends: end the pairing with %s: %v", id, err)
	}
	if removed {
		m.notifyChanged(SetChange{Action: SetRemoved, ID: id, Reason: RemovedByComputer})
	}
}
