package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
// desktop), so every change to the three files is one transition run by
// transact, and no other code writes any of them: the session file is
// written through the transaction's held session, and the two shared files
// only by computerTx's methods, which nothing outside a transaction can
// call. TestOnlyTheComputerTransactionWritesItsState is the backstop.
//
// The cached carrier is this process's alone. A lookup (carrier) fills an
// empty cache from the stored session without the session file's lock, so
// it can cache a pairing another manager is replacing; a cached carrier
// follows the file at its client's next session transaction, which retires
// a client whose session the file no longer holds, and the next lookup
// evicts a retired carrier.
//
// Lock order, for every transition, all held until it ends:
//
//  1. the computer's profile lock (Manager.profiles, in-process);
//  2. its session file's OS lock (deviceclient.WithSession), the
//     cross-process order;
//  3. the two shared files' in-process locks (Manager.sharedFiles), the
//     exclusions' then agent access's. Not Manager.profiles: a computer id
//     may spell a shared file's lock name, and its transition would then
//     wait on its own profile lock;
//  4. the two shared files' OS locks (deviceclient.WithProfileLock), in the
//     same order;
//  5. Manager.mu, a leaf: nothing waits while holding it
//     (TestNothingWaitsUnderTheManagerMutex).
//
// Every transition on any computer of the profile directory therefore runs
// one at a time; each reads and writes a few local files and nothing else.
//
// A client's mutex is only ever taken before a session file's lock
// (deviceclient's sessionTransaction), so a transition calls no client
// method that takes it (TestNoTransitionUsesAClient).
//
// Writes within a transition are ordered so that a crash between two leaves
// the safe side. A transition that fails after its first write attempt is
// rolled back by transact to the three files as it found them (rollBack):
// with every lock held throughout, no other transition can have read or
// written them meanwhile.
//
// The cache follows the files: a transition only records which carrier
// leaves (drop) and which arrives (install), and transact applies that once
// the session file is released, if and only if the transition committed. A
// failed or rolled-back transition leaves the cached carrier and its
// connections as they were. Applying under the profile lock is what fences
// a leaving carrier's verdict path: its client can see the new file only
// after the session file is released, and its endSession waits for the
// profile lock, by when it is no longer the cached carrier.

// computerTx is one computer's local pairing state held for one transition.
type computerTx struct {
	m    *Manager
	id   string
	held *deviceclient.HeldSession
	// exclusions and access are the shared files as the transaction found
	// them.
	exclusions, access sharedFile
	// wrote is set before the first write is attempted; committed once the
	// transition returned with its writes standing.
	wrote, committed bool
	// What transact applies to this process's cache if the transition
	// commits: the cached carrier leaves with dropping as its verdict, or is
	// replaced by arriving; verdict is the name an ended pairing leaves
	// (TakeVerdict).
	dropping error
	arriving *carrier
	verdict  *string
}

// txOutcome is what a transaction did, apart from the error it answers: a
// transition can commit and its locks still fail to release.
type txOutcome struct {
	// committed is whether the transition's writes stand, and so whether
	// its cache changes were applied.
	committed bool
	// installed is whether the carrier it installed is cached and renewing,
	// which it is not after Close.
	installed bool
}

// sharedFile is one shared file as a transaction found it: its bytes, or
// that it did not exist.
type sharedFile struct {
	path  string
	data  []byte
	found bool
}

// sharedFileLockTimeout bounds the wait for both shared files' OS locks.
const sharedFileLockTimeout = 5 * time.Second

// transact runs transition on id's local pairing state (computerTx). Its
// error is the transition's, or its locks' release; the outcome says
// whether the transition committed whichever it was.
func (m *Manager) transact(id string, transition func(*computerTx) error) (txOutcome, error) {
	unlock := m.profiles.Lock(id)
	defer unlock()
	tx := &computerTx{m: m, id: id}
	err := errors.Join(deviceclient.WithSession(m.dir, id, func(held *deviceclient.HeldSession) error {
		tx.held = held
		return m.withSharedFiles(func() error { return tx.run(transition) })
	}), m.releaseFault)
	if !tx.committed {
		return txOutcome{}, err
	}
	// Still under the profile lock, so no lookup builds another owner from
	// the file meanwhile.
	return txOutcome{committed: true, installed: m.applyCache(tx)}, err
}

// applyCache applies a committed transition's cache changes and reports
// whether the carrier it installed is cached.
func (m *Manager) applyCache(tx *computerTx) (installed bool) {
	var leaving *carrier
	verdict := tx.dropping
	m.mu.Lock()
	if tx.dropping != nil || tx.arriving != nil {
		leaving = m.carriers[tx.id]
		delete(m.carriers, tx.id)
	}
	if tx.verdict != nil {
		m.verdicts[tx.id] = *tx.verdict
	}
	if tx.arriving != nil {
		verdict = errPairingReplaced
		delete(m.verdicts, tx.id)
		if !m.closed {
			m.wire(tx.arriving, tx.id)
			m.carriers[tx.id] = tx.arriving
			installed = true
		}
	}
	m.mu.Unlock()
	if leaving != nil {
		leaving.leave(verdict)
		leaving.client.Retire()
	}
	return installed
}

// withSharedFiles runs fn holding both shared files' locks, in the order
// stated above.
func (m *Manager) withSharedFiles(fn func() error) error {
	unlockExclusions := m.sharedFiles.Lock(ownExclusionsLock)
	defer unlockExclusions()
	unlockAccess := m.sharedFiles.Lock(agentAccessLock)
	defer unlockAccess()
	ctx, cancel := context.WithTimeout(context.Background(), sharedFileLockTimeout)
	defer cancel()
	return deviceclient.WithProfileLock(ctx, m.dir, ownExclusionsLock, func() error {
		return deviceclient.WithProfileLock(ctx, m.dir, agentAccessLock, fn)
	})
}

// run reads the shared files as found, runs transition, and rolls back a
// transition that fails after a write was attempted.
func (tx *computerTx) run(transition func(*computerTx) error) error {
	var err error
	if tx.exclusions, err = readSharedFile(filepath.Join(tx.m.dir, ownExclusionsFile)); err != nil {
		return err
	}
	if tx.access, err = readSharedFile(filepath.Join(tx.m.dir, agentAccessFile)); err != nil {
		return err
	}
	if err = transition(tx); err != nil {
		if tx.wrote {
			return errors.Join(err, tx.rollBack())
		}
		return err
	}
	tx.committed = true
	return nil
}

func readSharedFile(path string) (sharedFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sharedFile{path: path}, nil
	}
	if err != nil {
		return sharedFile{}, fmt.Errorf("attachedbackends: read %s: %w", path, err)
	}
	return sharedFile{path: path, data: data, found: true}, nil
}

// rollBack puts the three files back as the transaction found them, in the
// order session, exclusions, agent access, each read back to confirm it: a
// write can take effect and still answer an error. The shared files are put
// back only once the session is, so an opt-in is never given back to a
// pairing other than the one it was given for; a crash part way leaves the
// forward writes, which take a grant away and never give one.
func (tx *computerTx) rollBack() error {
	restored, err := tx.held.Restore()
	if !restored {
		return fmt.Errorf("attachedbackends: the pairing with %s could not be put back, so its exclusion and agent access keep this change's writes: %w", tx.id, err)
	}
	return errors.Join(err, tx.restoreShared(tx.exclusions), tx.restoreShared(tx.access))
}

// restoreShared puts one shared file back as the transaction found it.
func (tx *computerTx) restoreShared(f sharedFile) error {
	holds := func() bool {
		data, err := os.ReadFile(f.path)
		if !f.found {
			return errors.Is(err, os.ErrNotExist)
		}
		return err == nil && string(data) == string(f.data)
	}
	if holds() {
		return nil
	}
	var err error
	if f.found {
		err = atomicfile.Write(f.path, f.data)
	} else {
		err = atomicfile.Remove(f.path)
	}
	if !holds() {
		return errors.Join(fmt.Errorf("attachedbackends: %s could not be put back", f.path), err)
	}
	return err
}

// stored is the session in the computer's file now.
func (tx *computerTx) stored() (deviceclient.Session, error) { return tx.held.Stored() }

// save writes an answered redemption with nickname (deviceclient.Redemption.Save).
func (tx *computerTx) save(redeemed *deviceclient.Redemption, nickname string) (*deviceclient.Client, deviceclient.Pairing, error) {
	tx.wrote = true
	return redeemed.Save(tx.held, nickname)
}

// removeSession deletes the stored session.
func (tx *computerTx) removeSession() error {
	tx.wrote = true
	return tx.held.Remove()
}

// cached is the carrier this process caches for the computer.
func (tx *computerTx) cached() *carrier {
	tx.m.mu.Lock()
	defer tx.m.mu.Unlock()
	return tx.m.carriers[tx.id]
}

// install has built replace whatever carrier is cached, and start
// renewing, if the transition commits (applyCache).
func (tx *computerTx) install(built *carrier) { tx.arriving = built }

// drop has the cached carrier leave with verdict if the transition commits
// (applyCache).
func (tx *computerTx) drop(verdict error) { tx.dropping = verdict }

// recordVerdict keeps name as the ended pairing's, for TakeVerdict, if the
// transition commits (applyCache).
func (tx *computerTx) recordVerdict(name string) { tx.verdict = &name }

// excluded reports whether the computer is excluded as an own device.
func (tx *computerTx) excluded() (bool, error) { return tx.m.ownDeviceExcluded(tx.id) }

// setExcluded records or clears the computer's own-device exclusion. It is
// the one writer of excluded-own-devices.json.
func (tx *computerTx) setExcluded(excluded bool) error {
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
	tx.wrote = true
	return atomicfile.WriteJSON(filepath.Join(tx.m.dir, ownExclusionsFile), entries)
}

// setAgentAccess turns the computer's agent opt-in on or off. It is the one
// writer of agent-access.json. Turning one on also prunes the opt-ins of
// computers with no session stored, which only a verdict leaves behind, and
// is refused past MaxAgentComputers; a rollback is not (rollBack).
func (tx *computerTx) setAgentAccess(enabled bool) error {
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
	tx.wrote = true
	return atomicfile.WriteJSON(filepath.Join(tx.m.dir, agentAccessFile), access)
}

// The shared files and the names of their locks.
const (
	ownExclusionsFile = "excluded-own-devices.json"
	ownExclusionsLock = "own-device-exclusions"
	agentAccessFile   = "agent-access.json"
	agentAccessLock   = "agent-access"
)

// commitEnrollment saves an answered redemption, unless the enrollment was
// cancelled, its caller has left, or the computer is not wanted any more
// (beginCommit, admitEnrollment), all decided in the transaction, so neither
// a forget nor a pairing another manager wrote since the check before the
// redemption is overwritten. The carrier is swapped only once the
// transition committed (applyCache): after a refusal or a failure the
// cached carrier and its connections stay as they were.
//
// A commit whose locks then fail to release answers that error with the
// attachment and the carrier installed: the pairing is saved, and the
// carrier is cached and renewing.
func (m *Manager) commitEnrollment(e *enrollment, link deviceclient.Link, redeemed *deviceclient.Redemption) (Attachment, *carrier, error) {
	var attachment Attachment
	var built *carrier
	outcome, err := m.transact(link.BackendID, func(tx *computerTx) error {
		if err := m.beginCommit(e); err != nil {
			return err
		}
		stored, loadErr := tx.stored()
		if err := m.admitEnrollment(link.BackendID, e, stored, loadErr); err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return err
		}
		var pairing deviceclient.Pairing
		var err error
		built, pairing, err = m.savePairing(tx, e.purpose == introductionByPeer, redeemed, stored, loadErr)
		if err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return err
		}
		saved, err := tx.stored()
		if err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return err
		}
		tx.install(built)
		attachment = Attachment{
			ID:                 link.BackendID,
			Name:               displayName(saved),
			Endpoint:           link.Endpoint,
			VerificationNumber: pairing.VerificationNumber,
		}
		return nil
	})
	if !outcome.committed {
		return Attachment{}, nil, err
	}
	if !outcome.installed {
		built = nil
	}
	return attachment, built, err
}

// savePairing makes an enrollment's writes and builds its carrier, in
// order; a failure part way is rolled back by its transaction.
//
// A person's pairing is a new grant. Its steps: agent access off, the
// session (no nickname), the carrier built, the exclusion cleared. A crash
// after the session leaves the computer paired and still excluded, so no
// automatic introduction follows. An introduction writes only the session,
// keeping agent access and the stored nickname: it upgrades a pairing, it
// does not grant one.
func (m *Manager) savePairing(tx *computerTx, introduction bool, redeemed *deviceclient.Redemption, stored deviceclient.Session, loadErr error) (*carrier, deviceclient.Pairing, error) {
	nickname, wasExcluded := "", false
	if introduction {
		if loadErr == nil {
			nickname = stored.Nickname
		}
	} else {
		var err error
		if wasExcluded, err = tx.excluded(); err != nil {
			return nil, deviceclient.Pairing{}, err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return nil, deviceclient.Pairing{}, err
		}
	}
	client, pairing, err := tx.save(redeemed, nickname)
	if err != nil {
		return nil, deviceclient.Pairing{}, err
	}
	built, err := m.buildCarrier(client, tx.id)
	if err != nil {
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
// paired, never paired with the exclusion gone. A removal that fails is
// rolled back by its transaction and leaves the cached carrier working. A
// removal that committed is announced even if its locks then fail to
// release, and that error is returned.
func (m *Manager) Remove(id string) error {
	m.cancelEnrollment(id, errPairingForgotten)
	outcome, err := m.transact(id, func(tx *computerTx) error {
		if err := tx.setExcluded(true); err != nil {
			return err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return err
		}
		tx.drop(errPairingForgotten)
		return tx.removeSession()
	})
	if outcome.committed {
		m.notifyChanged(SetChange{Action: SetRemoved, ID: id})
	}
	return err
}

// retireOwnProfile forgets one own-device profile the way a tombstone does,
// if it holds the session named (any session when sessionID is empty), and
// announces it as SetRemoved, as every removal is. Ordinary limited shares
// are separate grants and are never touched, and neither is a pairing
// another manager saved in place of the named one. Its writes, in order:
// agent access off, the session deleted. It reports whether it forgot the
// profile, which it may have done and still answer a lock-release error.
func (m *Manager) retireOwnProfile(id, sessionID string) (bool, error) {
	dropped := false
	outcome, err := m.transact(id, func(tx *computerTx) error {
		stored, err := tx.stored()
		if errors.Is(err, deviceclient.ErrNoSession) {
			return nil
		}
		if err != nil || !stored.OwnDevice || (sessionID != "" && stored.SessionID != sessionID) {
			return err
		}
		if err := tx.setAgentAccess(false); err != nil {
			return err
		}
		tx.drop(errPairingForgotten)
		if err := tx.removeSession(); err != nil {
			return err
		}
		dropped = true
		return nil
	})
	if !outcome.committed || !dropped {
		return false, err
	}
	m.notifyChanged(SetChange{Action: SetRemoved, ID: id})
	return true, err
}

// SetAgentAccess turns one computer's agent opt-in on or off. Turning it
// on needs a session stored for the computer.
func (m *Manager) SetAgentAccess(id string, enabled bool) error {
	if !entityid.Valid(id) {
		return errors.New("invalid computer identity")
	}
	_, err := m.transact(id, func(tx *computerTx) error {
		if enabled {
			if _, err := tx.stored(); err != nil {
				return err
			}
		}
		return tx.setAgentAccess(enabled)
	})
	return err
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
	outcome, err := m.transact(id, func(tx *computerTx) error {
		if tx.cached() != held {
			return nil
		}
		tx.drop(errPairingEnded)
		if _, err := tx.stored(); !errors.Is(err, deviceclient.ErrNoSession) {
			return err
		}
		tx.recordVerdict(name)
		removed = true
		return tx.setAgentAccess(false)
	})
	if err != nil {
		// Nobody is waiting on a verdict; the log is where it surfaces.
		log.Printf("attachedbackends: end the pairing with %s: %v", id, err)
	}
	if outcome.committed && removed {
		m.notifyChanged(SetChange{Action: SetRemoved, ID: id, Reason: RemovedByComputer})
	}
}
