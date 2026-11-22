package attachedbackends

import (
	"context"
	"errors"

	"agent-overflow/internal/deviceclient"
)

// enrollment is one pairing link being spent for a computer: a person's
// pairing (Add) or an own-device introduction. At most one exists per
// computer (Manager.enrollments). Its redemption runs with no profile lock
// held; the lock is taken once the redemption has answered, only to check
// the computer is still wanted and commit the session.
type enrollment struct {
	purpose enrollmentPurpose
	// unpairedOnly is a person's pairing by address, which must not replace
	// a pairing this computer already holds (admitEnrollment).
	unpairedOnly bool
	// state is guarded by Manager.mu and changes only through moveEnrollment.
	state enrollmentState
	// ctx is what the redemption runs on. It ends when the enrollment is
	// cancelled, with the reason as its cause.
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// enrollmentPurpose is who asked for an enrollment, which decides what
// ends it.
type enrollmentPurpose int

const (
	// pairingByPerson is a pairing someone started here. Forget cancels it.
	// Shutdown does not once its redemption has been sent: a spent link for
	// a person heals only by pairing again, and the other computer would
	// keep a device nobody holds, so the redemption finishes and is saved.
	pairingByPerson enrollmentPurpose = iota
	// introductionByPeer is an own-device introduction. Forget and the
	// start of shutdown cancel it: the next introduction the other computer
	// mints for this device revokes a session nobody holds
	// (store.RetireOwnIntroductions), and the next sync asks for one.
	introductionByPeer
)

// enrollmentState is where an enrollment stands. The moves, each made under
// Manager.mu by moveEnrollment:
//
//	redeeming -> cancelled  Forget, or for an introduction the start of
//	                        shutdown. The redemption's context ends; an
//	                        answer that still arrives is not saved.
//	redeeming -> failed     The redemption failed, or the computer was no
//	                        longer wanted. Nothing was saved.
//	redeeming -> committed  The redemption answered and, under the
//	                        profile lock, the computer is still wanted.
//	                        Nothing cancels it from here.
//	committed -> failed     The session could not be saved.
//
// cancelled and failed are final. An enrollment leaves Manager.enrollments
// when the call that began it returns.
type enrollmentState int

const (
	enrollmentRedeeming enrollmentState = iota
	enrollmentCancelled
	enrollmentCommitted
	enrollmentFailed
)

var (
	// errEnrollmentInProgress answers a person's pairing with a computer
	// that is already being paired or introduced.
	errEnrollmentInProgress = errors.New("a pairing with this computer is already in progress")
	// errShuttingDown is the manager's life's cause once shutdown began
	// (BeginClose), and why an enrollment it cancelled ended.
	errShuttingDown = errors.New("attachedbackends: this computer is quitting")
	// errNotWanted is an introduction to a computer that was forgotten here
	// or already holds an own-device pairing. Its caller answers it as
	// not added, with no error.
	errNotWanted = errors.New("attachedbackends: the introduction is no longer wanted")
	// errAlreadyConnected refuses a pairing by address with a computer this
	// one already holds a pairing with.
	errAlreadyConnected = errors.New("this computer is already connected; use its existing connection")
)

// beginEnrollment claims id for one enrollment, and refuses while another
// enrollment for id is in flight. One begun after shutdown began is not
// cancelled by it; its redemption is never sent (deviceclient.Redeem).
func (m *Manager) beginEnrollment(id string, purpose enrollmentPurpose, unpairedOnly bool) (*enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enrollments[id] != nil {
		if purpose == introductionByPeer {
			return nil, errNotWanted
		}
		return nil, errEnrollmentInProgress
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	e := &enrollment{purpose: purpose, unpairedOnly: unpairedOnly, state: enrollmentRedeeming, ctx: ctx, cancel: cancel}
	m.enrollments[id] = e
	return e, nil
}

// finishEnrollment releases id's claim once the call that began e returns.
func (m *Manager) finishEnrollment(id string, e *enrollment) {
	m.mu.Lock()
	if m.enrollments[id] == e {
		delete(m.enrollments, id)
	}
	m.mu.Unlock()
	e.cancel(nil)
}

// moveEnrollment moves e from one state to another and reports whether e
// was in from.
func (m *Manager) moveEnrollment(e *enrollment, from, to enrollmentState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moveEnrollmentLocked(e, from, to)
}

func (m *Manager) moveEnrollmentLocked(e *enrollment, from, to enrollmentState) bool {
	if e.state != from {
		return false
	}
	e.state = to
	return true
}

// cancelEnrollment cancels id's enrollment in flight, if there is one, for
// cause.
func (m *Manager) cancelEnrollment(id string, cause error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.enrollments[id]; e != nil {
		m.cancelEnrollmentLocked(e, cause)
	}
}

// cancelEnrollmentLocked cancels e for cause if its redemption has not been
// committed. Caller holds m.mu.
func (m *Manager) cancelEnrollmentLocked(e *enrollment, cause error) {
	if m.moveEnrollmentLocked(e, enrollmentRedeeming, enrollmentCancelled) {
		e.cancel(cause)
	}
}

// admitEnrollment says whether e's computer may be enrolled. It is checked
// before the redemption and again under the profile lock, and reads only
// local files.
func (m *Manager) admitEnrollment(id string, e *enrollment) error {
	if e.purpose == introductionByPeer {
		wanted, err := m.introductionWanted(id)
		if err == nil && !wanted {
			err = errNotWanted
		}
		return err
	}
	if _, err := deviceclient.LoadSession(m.dir, id); e.unpairedOnly && err == nil {
		return errAlreadyConnected
	}
	return nil
}

// enroll spends link for one enrollment of purpose and commits what it
// returns (admitEnrollment). The carrier is the one installed, nil after
// Close.
func (m *Manager) enroll(link deviceclient.Link, purpose enrollmentPurpose, unpairedOnly bool) (Attachment, *carrier, error) {
	e, err := m.beginEnrollment(link.BackendID, purpose, unpairedOnly)
	if err != nil {
		return Attachment{}, nil, err
	}
	defer m.finishEnrollment(link.BackendID, e)
	if err := m.admitEnrollment(link.BackendID, e); err != nil {
		m.moveEnrollment(e, enrollmentRedeeming, enrollmentFailed)
		return Attachment{}, nil, err
	}
	label, err := m.localLabel()
	if err != nil {
		m.moveEnrollment(e, enrollmentRedeeming, enrollmentFailed)
		return Attachment{}, nil, err
	}
	redeemed, err := deviceclient.Redeem(e.ctx, m.dir, link, label, m.platform, m.clientOptions()...)
	if err != nil {
		if !m.moveEnrollment(e, enrollmentRedeeming, enrollmentFailed) {
			return Attachment{}, nil, errors.Join(context.Cause(e.ctx), err)
		}
		return Attachment{}, nil, err
	}
	return m.commitEnrollment(e, link, redeemed)
}

// commitEnrollment saves an answered redemption under the destination's
// profile lock, unless the enrollment was cancelled or the computer is no
// longer wanted, and swaps the old carrier for the new one at the save:
// until then the old one keeps working. A person's pairing is a new grant:
// it starts with agent access off and clears a forget of the computer as an
// own device.
func (m *Manager) commitEnrollment(e *enrollment, link deviceclient.Link, redeemed *deviceclient.Redemption) (Attachment, *carrier, error) {
	unlock := m.profiles.Lock(link.BackendID)
	defer unlock()
	if err := m.admitEnrollment(link.BackendID, e); err != nil {
		if !m.moveEnrollment(e, enrollmentRedeeming, enrollmentFailed) {
			return Attachment{}, nil, context.Cause(e.ctx)
		}
		return Attachment{}, nil, err
	}
	if !m.moveEnrollment(e, enrollmentRedeeming, enrollmentCommitted) {
		return Attachment{}, nil, context.Cause(e.ctx)
	}
	if e.purpose == pairingByPerson {
		// Do not revive an old agent opt-in after revocation or an
		// incomplete pairing; the explicit enable follows it. An
		// introduction that upgrades an already-paired computer to a group
		// session keeps a live opt-in.
		if err := m.writeAgentAccess(link.BackendID, false); err != nil {
			m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
			return Attachment{}, nil, err
		}
	}
	m.retireCarrier(link.BackendID)
	client, pairing, err := redeemed.Save()
	if err != nil {
		m.moveEnrollment(e, enrollmentCommitted, enrollmentFailed)
		return Attachment{}, nil, err
	}
	return m.installLocked(e, link, client, pairing)
}

// installLocked caches and renews the carrier for a session just saved.
// Caller holds the destination's profile lock.
func (m *Manager) installLocked(e *enrollment, link deviceclient.Link, client *deviceclient.Client, pairing deviceclient.Pairing) (Attachment, *carrier, error) {
	built, err := newCarrier(client, link.BackendID)
	if err != nil {
		return Attachment{}, nil, err
	}
	m.mu.Lock()
	// After Close the pairing stays on disk and nothing is cached or renewed.
	installed := !m.closed
	if installed {
		m.wire(built, link.BackendID)
		m.carriers[link.BackendID] = built
	}
	delete(m.verdicts, link.BackendID)
	m.mu.Unlock()
	if !installed {
		built = nil
	}
	if e.purpose == pairingByPerson {
		if err := m.excludeOwnDevice(link.BackendID, false); err != nil {
			return Attachment{}, nil, err
		}
	}
	return Attachment{
		ID:                 link.BackendID,
		Name:               displayName(client.Session()),
		Endpoint:           link.Endpoint,
		VerificationNumber: pairing.VerificationNumber,
	}, built, nil
}

// retireCarrier takes id's cached carrier out of the cache and retires it,
// because the session behind it is about to be replaced. The file stays
// until the replacement is saved. Caller holds id's profile lock and not
// m.mu: the old carrier's observers run as it leaves.
func (m *Manager) retireCarrier(id string) {
	m.mu.Lock()
	old := m.carriers[id]
	delete(m.carriers, id)
	m.mu.Unlock()
	if old != nil {
		old.leave(errPairingReplaced)
		old.client.Retire()
	}
}
