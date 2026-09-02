package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/push"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The phone half of the notification path (docs/specs/remote-access.md §9,
// "Push"). `internal/notify` decides WHAT a moment says, `notifyOS` presents
// it on the machine this process runs on, and this file wakes the phones
// that were not looking.
//
// IT HANGS OFF queueNotification, NOT OFF A SECOND TAP. One mapping, two
// audiences: the desktop presenter and the fan-out see the exact same
// notify.Send, so a phone can never be told something the desktop was not,
// and a retraction reaches both or neither.
//
// ITS OWN QUEUE, THOUGH. The desktop's toast is local and fast; a send to
// Google is a TLS round trip to another continent that can hang for the
// full ten seconds of its timeout. Sharing the notification queue would
// mean one slow send delaying the next turn's toast on the machine the
// person is sitting at. So: two queues, each serial for the same reason —
// ORDER is the retraction contract, and a retract that overtook its own
// send would leave a notification on a lock screen forever.
//
// OWNER-ONLY, THIS WAVE (user ruling 2026-09-01). The owner's backend holds
// a credential and sends; a friend's backend holds none, so its Sender is
// nil and it records registrations without sending. That nil check is the
// whole of the difference, which is what lets the designed next step — the
// home backend as a wake relay for the backends attached to it — arrive as
// a different Sender and change no caller here.

// pushDispatch is the App's push coordination: the ordered queue sends run
// on, the sender they run through, and what the owner is shown about it.
//
// loggedKinds is touched ONLY from queue jobs, which run one at a time with
// a happens-before edge between them, so it needs no lock — the same
// arrangement notificationDispatch's ledger has. Everything under mu is
// read by RPCs on other goroutines and does.
type pushDispatch struct {
	queue serialqueue.Queue

	mu          sync.Mutex
	sender      push.Sender
	projectID   string
	clientEmail string
	// lastError is the first failure since the last success, which is what
	// makes a dead credential visible from the phone that stopped being
	// woken. Cleared by a success, so it never outlives the fault.
	lastError string

	loggedKinds map[notify.Kind]struct{}
}

// pushDrainTimeout bounds shutdown's wait for the queue. Longer than the
// notification drain because the work is a network round trip rather than a
// D-Bus call, and short enough that an unreachable Google cannot hold a quit
// open: one in-flight send is worth finishing, a backlog is not.
const pushDrainTimeout = 3 * time.Second

// loadPushSender installs the sender this backend sends with, from the
// credential the owner pasted. Called once at boot, after the store opens.
//
// No credential is the RESTING STATE of every backend but the owner's own,
// so an absent row is not a failure and says nothing in the log. A row that
// will not parse IS worth a line: the owner pasted something, and the only
// other evidence would be phones that quietly stop buzzing.
func (a *App) loadPushSender() {
	if a.store == nil {
		return
	}
	cred, err := a.store.GetPushSender()
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("push: read the sender credential: %v", err)
		}
		return
	}
	parsed, err := push.ParseCredential([]byte(cred.CredentialJSON))
	if err != nil {
		log.Printf("push: the stored sender credential is unusable: %v", err)
		return
	}
	sender, err := push.NewFCMSender(parsed)
	if err != nil {
		log.Printf("push: build the sender: %v", err)
		return
	}
	a.installPushSender(sender, parsed.ProjectID, parsed.ClientEmail)
}

// installPushSender swaps the sender live. The queue may be mid-send with
// the previous one, which is fine: a Sender is immutable once built, and the
// job holds its own reference.
func (a *App) installPushSender(sender push.Sender, projectID, clientEmail string) {
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	a.push.sender = sender
	a.push.projectID = projectID
	a.push.clientEmail = clientEmail
	a.push.lastError = ""
}

// currentPushSender answers the sender and the name a message travels under,
// in one lock.
func (a *App) currentPushSender() push.Sender {
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	return a.push.sender
}

// pushFanout wakes every live registration for one mapped moment.
//
// Called from queueNotification's job, on the NOTIFICATION queue, so it must
// not do the work inline — it hands it to its own queue and returns.
//
// The two early returns are the cheap ones, and they are checked in this
// order on purpose: a backend with no credential is the common case (every
// friend's backend, and the owner's until they paste one), and it must cost
// nothing at all — not a queue job, not a SQLite read. THE COMMENT THAT
// MATTERS: when the home backend becomes a wake relay for the backends
// attached to it (§18 item 1), that backend gets a forwarding Sender here
// and this branch simply stops being taken. Nothing downstream changes.
func (a *App) pushFanout(send notify.Send) {
	if a.store == nil {
		return
	}
	sender := a.currentPushSender()
	if sender == nil {
		return
	}
	name := backendDisplayName()
	a.push.queue.Go(func() { a.deliverPush(sender, name, send) })
}

// deliverPush is the fan-out body, on the push queue.
func (a *App) deliverPush(sender push.Sender, backendName string, send notify.Send) {
	tokens, err := a.store.LivePushTokens()
	if err != nil {
		a.recordPushFailure(send.Kind, fmt.Errorf("read push registrations: %w", err))
		return
	}
	for _, token := range tokens {
		if !a.pushAllowed(token, send) {
			continue
		}
		message, err := push.MessageFor(send, token.Token, backendName)
		if err != nil {
			// The mapping produced something the contract refuses. It is
			// the same message for every device, so one report is the
			// whole story and the loop has nothing left to do.
			a.recordPushFailure(send.Kind, err)
			return
		}
		a.sendOnePush(sender, token, message, send.Kind)
	}
}

// pushAllowed is the per-device preference gate: the same question
// notifyOS asks about the machine this process runs on, asked about a
// phone, through that phone's own device-tier bucket.
//
// A RETRACTION IS NEVER GATED, for the reason notifyOS states: withdrawing
// something already on a lock screen is the opposite of an interruption,
// and gating it would let a toggle strand the very alerts it was flipped to
// stop.
func (a *App) pushAllowed(token store.PushToken, send notify.Send) bool {
	if send.Retract {
		return true
	}
	current := settings.DefaultSettings
	if a.settings != nil {
		current = a.settings.For("device:"+token.DeviceID, settingsDeviceClass(token.DeviceClass)).Get()
	}
	return notificationKindEnabledIn(current, send.Kind)
}

// sendOnePush delivers to one device and reacts to the one failure that has
// a reaction.
func (a *App) sendOnePush(sender push.Sender, token store.PushToken, message push.Message, kind notify.Kind) {
	err := sender.Send(context.Background(), message)
	switch {
	case err == nil:
		a.notePushSuccess()
	case errors.Is(err, push.ErrTokenGone):
		// The registration is dead, not the credential. Drop the row and
		// say nothing: the phone mints a new token and re-registers on its
		// next launch, so this is routine rather than a fault, and
		// recording it as lastError would show the owner a problem that
		// has already fixed itself.
		if _, delErr := a.store.DeletePushToken(token.DeviceID); delErr != nil {
			log.Printf("push: drop the dead registration for device %s: %v", token.DeviceID, delErr)
		}
	default:
		a.recordPushFailure(kind, err)
	}
}

// notePushSuccess clears the standing fault. A send that worked is the only
// evidence that whatever was wrong no longer is.
func (a *App) notePushSuccess() {
	a.push.mu.Lock()
	a.push.lastError = ""
	a.push.mu.Unlock()
}

// recordPushFailure keeps the first failure since the last success for the
// owner to read, and logs ONCE per kind.
//
// The log-once ledger mirrors logNotificationFailure's argument: a bad
// credential fails every send, and one line says that as well as a line per
// completed turn for the rest of the day. It is keyed by notification KIND
// rather than by error, because the kinds are a closed set of six while the
// errors Google returns are not.
func (a *App) recordPushFailure(kind notify.Kind, err error) {
	if err == nil {
		return
	}
	a.push.mu.Lock()
	if a.push.lastError == "" {
		a.push.lastError = err.Error()
	}
	a.push.mu.Unlock()

	if a.push.loggedKinds == nil {
		a.push.loggedKinds = make(map[notify.Kind]struct{}, 6)
	}
	if _, logged := a.push.loggedKinds[kind]; logged {
		return
	}
	a.push.loggedKinds[kind] = struct{}{}
	log.Printf("push: %v (further %s failures are not logged)", err, kind)
}

// drainPush lets in-flight sends finish before the process goes away, beside
// the notification drain and for the same reason: the notification about a
// turn that completed during teardown is exactly the one somebody walked
// away from.
func (a *App) drainPush(ctx context.Context, timeout time.Duration) error {
	drainCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.push.queue.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
}

// PushSenderStatus is what the owner is shown about this backend's ability
// to wake a phone.
//
// The credential itself is NOT here and cannot be: it is backend-local
// secret material of the same class as `signing_keys.secret`, and this shape
// is read by an admin device that is not at the machine. The project and the
// service account are what identify it; `LastError` is what says whether it
// still works.
type PushSenderStatus struct {
	Configured        bool   `json:"configured"`
	ProjectID         string `json:"projectId"`
	ClientEmail       string `json:"clientEmail"`
	LastError         string `json:"lastError"`
	RegisteredDevices int    `json:"registeredDevices"`
}

// callerPushDevice resolves the device a push registration belongs to: the
// one behind the CALLING session, never a parameter.
//
// A device id that came in as an argument would let any session register a
// token against any device, which is a way to have somebody else's phone
// woken by this backend. There is no reason for such a parameter to exist,
// so it does not.
//
// A connection on the local page channel is refused. That channel names the
// BACKEND's own channel rather than one screen — the embedded webview, the
// WSL relay and a `--connect` stub all present the single session this
// backend minted for itself — so there is no one device to attribute a
// registration to. A phone is a paired device or it is not registering.
func (a *App) callerPushDevice(ctx context.Context) (store.Device, error) {
	sessionID := transport.SessionFromContext(ctx)
	if sessionID == "" {
		return store.Device{}, fmt.Errorf("push: registration is for a call made from a paired device's session")
	}
	state := a.identityState()
	if state == nil {
		return store.Device{}, fmt.Errorf("push: identity is not wired")
	}
	session, reason := state.sessions.Live(sessionID)
	if reason.Refused() {
		return store.Device{}, transport.AuthRefused(reason.Code())
	}
	device, err := a.store.GetDevice(session.DeviceID)
	if err != nil {
		return store.Device{}, fmt.Errorf("push: read the session's device: %w", err)
	}
	if device.Channel == identity.LocalChannel {
		return store.Device{}, fmt.Errorf("push: the local page channel is not one device, so it cannot register for push")
	}
	return device, nil
}

// RegisterPushToken records the platform registration token of the calling
// device, so this backend can wake it.
//
// At the SESSION FLOOR, on the same argument as the ui_state calls: the
// authority is decided by the caller's own identity rather than by a grant,
// because this call reaches exactly one row — the calling device's — and no
// other. A session that was granted nothing still owns its own phone.
//
//ao:scope session
//ao:route home
func (a *App) RegisterPushToken(ctx context.Context, platform, token string) error {
	device, err := a.callerPushDevice(ctx)
	if err != nil {
		return err
	}
	return a.store.UpsertPushToken(device.ID, platform, token, time.Now().UnixMilli())
}

// UnregisterPushToken forgets the calling device's registration.
//
// The shell calls this before it closes the socket to a backend it is
// detaching, which is the only moment it still can: the registration lives
// on the backend, and a device that has already dropped the connection has
// no way to say "stop waking me".
//
//ao:scope session
//ao:route home
func (a *App) UnregisterPushToken(ctx context.Context) error {
	device, err := a.callerPushDevice(ctx)
	if err != nil {
		return err
	}
	_, err = a.store.DeletePushToken(device.ID)
	return err
}

// SetPushSenderCredential installs the service-account key this backend
// sends with.
//
// `host` because it is the owner configuring their own machine, and
// //ao:stepup because it is a write behind a button rather than something a
// page does on its own — the same posture every other credential-shaped
// write on this surface has.
//
// VALIDATION IS SHAPE ONLY. Minting a token against the key would be the
// stronger check and it is deliberately not done: it would put a network
// call inside an RPC that must be testable, and this repo's tests reach no
// network. The first real send reports the rest, through
// GetPushSenderStatus's lastError, which is the surface the owner is already
// watching.
//
//ao:scope host
//ao:route home
//ao:stepup
func (a *App) SetPushSenderCredential(credentialJSON string) error {
	if a.store == nil {
		return fmt.Errorf("push: the store is unavailable")
	}
	parsed, err := push.ParseCredential([]byte(credentialJSON))
	if err != nil {
		return err
	}
	sender, err := push.NewFCMSender(parsed)
	if err != nil {
		return err
	}
	if err := a.store.SetPushSender(store.PushSenderCredential{
		ProjectID:      parsed.ProjectID,
		ClientEmail:    parsed.ClientEmail,
		CredentialJSON: string(parsed.Raw()),
		UpdatedAt:      time.Now().UnixMilli(),
	}); err != nil {
		return err
	}
	a.installPushSender(sender, parsed.ProjectID, parsed.ClientEmail)
	return nil
}

// ClearPushSenderCredential stops this backend sending.
//
// The REGISTRATIONS are deliberately left alone. A credential can be
// replaced in a minute, and dropping every phone's token with it would cost
// each of them a launch to come back — while the phones themselves have not
// changed their minds about being woken.
//
//ao:scope host
//ao:route home
//ao:stepup
func (a *App) ClearPushSenderCredential() error {
	if a.store == nil {
		return fmt.Errorf("push: the store is unavailable")
	}
	if _, err := a.store.ClearPushSender(); err != nil {
		return err
	}
	a.installPushSender(nil, "", "")
	return nil
}

// GetPushSenderStatus answers whether this backend can wake a phone, and
// how many are registered.
//
// `access:admin` rather than `host`: this is the one push call an owner
// needs to be able to make from somewhere else. "My phone stopped buzzing"
// is answered by this shape, and answering it should not require walking to
// the machine.
//
//ao:scope access:admin
//ao:route home
func (a *App) GetPushSenderStatus() (PushSenderStatus, error) {
	if a.store == nil {
		return PushSenderStatus{}, fmt.Errorf("push: the store is unavailable")
	}
	tokens, err := a.store.LivePushTokens()
	if err != nil {
		return PushSenderStatus{}, err
	}
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	return PushSenderStatus{
		Configured:        a.push.sender != nil,
		ProjectID:         a.push.projectID,
		ClientEmail:       a.push.clientEmail,
		LastError:         a.push.lastError,
		RegisteredDevices: len(tokens),
	}, nil
}
