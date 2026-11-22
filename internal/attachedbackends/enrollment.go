package attachedbackends

import (
	"context"
	"errors"

	"agent-overflow/internal/deviceclient"
)

// enrollment is one pairing link being spent for a computer: a person's
// pairing (Add) or an own-device introduction. At most one exists per
// computer (Manager.enrollments). Its redemption runs with no lock held;
// once it has answered, the computer's transaction (transact) saves the
// session only if the computer is still wanted (commitEnrollment).
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
	// done is closed once the enrollment has left Manager.enrollments.
	// awaitEnrollment waits on it.
	done chan struct{}
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
	// introductionByPeer is an own-device introduction. Forget, the start
	// of shutdown and its caller leaving cancel it: the next introduction
	// the other computer mints for this device revokes a session nobody
	// holds (store.RetireOwnIntroductions), and the next sync asks for one.
	introductionByPeer
)

// enrollmentState is where an enrollment stands. The moves, each made under
// Manager.mu by moveEnrollment:
//
//	redeeming -> cancelled  Forget, or for an introduction the start of
//	                        shutdown or its caller leaving. The
//	                        redemption's context ends; an answer that still
//	                        arrives is not saved.
//	redeeming -> failed     The redemption failed, or the computer was not
//	                        wanted before it was sent. Nothing was saved.
//	redeeming -> committed  The redemption answered, inside the computer's
//	                        transaction. Nothing cancels it from here.
//	committed -> failed     The transaction refused the computer as no
//	                        longer wanted (admitEnrollment), or a write
//	                        failed and was rolled back.
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

// beginEnrollment claims id for one enrollment. While another enrollment for
// id is in flight, a person's pairing is refused and an introduction waits
// for it to end (awaitEnrollment), then claims id and decides on what it
// left. A waiting introduction holds no claim, so nothing waits on it; its
// wait ends with ctx, which its caller ends at shutdown. One whose wait
// ended in a Forget is not wanted. One begun after shutdown began is not
// cancelled by it; its redemption is never sent (deviceclient.Redeem).
func (m *Manager) beginEnrollment(ctx context.Context, id string, purpose enrollmentPurpose, unpairedOnly bool) (*enrollment, error) {
	for {
		m.mu.Lock()
		if m.enrollments[id] == nil {
			ctx, cancel := context.WithCancelCause(context.Background())
			e := &enrollment{purpose: purpose, unpairedOnly: unpairedOnly, state: enrollmentRedeeming, ctx: ctx, cancel: cancel, done: make(chan struct{})}
			m.enrollments[id] = e
			m.mu.Unlock()
			return e, nil
		}
		m.mu.Unlock()
		if purpose == pairingByPerson {
			return nil, errEnrollmentInProgress
		}
		ended, err := m.awaitEnrollment(ctx, id)
		if err != nil {
			return nil, err
		}
		if ended != nil && errors.Is(context.Cause(ended.ctx), errPairingForgotten) {
			return nil, errNotWanted
		}
	}
}

// awaitEnrollment waits until no enrollment for id is in flight, or ctx
// ends, and returns the last one it waited for. Each is bounded by its
// redemption (the HTTP timeout) and its save's lock waits; Forget and, for
// an introduction, the start of shutdown end it sooner.
func (m *Manager) awaitEnrollment(ctx context.Context, id string) (*enrollment, error) {
	var ended *enrollment
	for {
		m.mu.Lock()
		e := m.enrollments[id]
		m.mu.Unlock()
		if e == nil {
			return ended, nil
		}
		select {
		case <-e.done:
			ended = e
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// finishEnrollment releases id's claim once the call that began e returns.
func (m *Manager) finishEnrollment(id string, e *enrollment) {
	m.mu.Lock()
	if m.enrollments[id] == e {
		delete(m.enrollments, id)
	}
	m.mu.Unlock()
	e.cancel(nil)
	close(e.done)
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

// admitEnrollment says whether e's computer may be enrolled, given the
// session stored for it as LoadSession answers it. It decides inside the
// computer's transaction (commitEnrollment), where another process cannot
// write between it and the save, and runs once before the redemption so a
// computer already refused spends no link. It reads only local files.
func (m *Manager) admitEnrollment(id string, e *enrollment, stored deviceclient.Session, loadErr error) error {
	if e.purpose == introductionByPeer {
		wanted, err := m.introductionWantedFor(id, stored, loadErr)
		if err == nil && !wanted {
			err = errNotWanted
		}
		return err
	}
	if e.unpairedOnly && loadErr == nil {
		return errAlreadyConnected
	}
	return nil
}

// enroll spends link for one enrollment of purpose and commits what it
// returns (commitEnrollment). For an introduction ctx is its caller's: it
// bounds the wait for another enrollment with the same computer
// (beginEnrollment), and its end cancels the redemption. A person's pairing
// ignores it. The carrier is the one installed, nil after Close.
func (m *Manager) enroll(ctx context.Context, link deviceclient.Link, purpose enrollmentPurpose, unpairedOnly bool) (Attachment, *carrier, error) {
	e, err := m.beginEnrollment(ctx, link.BackendID, purpose, unpairedOnly)
	if err != nil {
		return Attachment{}, nil, err
	}
	defer m.finishEnrollment(link.BackendID, e)
	if purpose == introductionByPeer {
		stop := context.AfterFunc(ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.cancelEnrollmentLocked(e, context.Cause(ctx))
		})
		defer stop()
	}
	stored, loadErr := deviceclient.LoadSession(m.dir, link.BackendID)
	if err := m.admitEnrollment(link.BackendID, e, stored, loadErr); err != nil {
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
