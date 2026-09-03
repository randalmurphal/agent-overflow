package app

import (
	"errors"
	"log"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/eventchan"
)

// The attached-backend admin surface: adding, naming and removing the
// OTHER machines this installation drives (docs/specs/remote-access.md
// §10).
//
// All four are `host` scope, which is what makes them local-machine-only
// without a second rule: host presence is the only key that opens a host
// method, and no session grant does. That matters more here than almost
// anywhere else on the wire — a caller that could attach a backend could
// point this installation at a machine of its choosing, and a caller that
// could list them learns every computer this person works on.
//
// All four are `home` route: they act on THIS backend's own profile
// directory. Asked of an attached backend they would answer about that
// machine's attachments, which is a real thing to want one day and not
// what this surface is.

// AttachedBackend is one attached machine. Aliased to the canonical shape
// so the wire type and the stored one cannot drift.
type AttachedBackend = attachedbackends.Attached

// BackendAttachment is what AddBackend answers.
type BackendAttachment = attachedbackends.Attachment

// BackendAttachOutcome is the frame the backend:attach channel carries:
// how one pairing ended, minutes after the RPC that started it returned.
type BackendAttachOutcome struct {
	ID string `json:"id"`
	// Attached is true when the owner of the far machine confirmed the
	// number. False means it ended some other way, and Error says how.
	Attached bool `json:"attached"`
	// Error is the reason, empty on success. Carried as text because
	// there is nothing for a client to branch on: every failure here ends
	// with the same remedy, which is to pair again.
	Error string `json:"error,omitempty"`
}

// BackendSetChange is the frame the backend:set-changed channel carries:
// every mutation of the attached-machine SET that is not the end of a
// pairing ceremony.
//
// Its own channel rather than a second meaning on backend:attach, because
// the two answer different questions — "how did the pairing I started end"
// and "the list moved" — and a receiver that conflated them would retire a
// pending row on a rename. Two pages open on this host, and the same page
// after a reload, converge on it.
type BackendSetChange struct {
	// Action is `removed` or `renamed`.
	Action string `json:"action"`
	ID     string `json:"id"`
	// Nickname is what this installation now calls the machine, empty when
	// the name was cleared or the row was removed.
	Nickname string `json:"nickname,omitempty"`
}

// Backend set actions.
const (
	BackendSetRemoved = "removed"
	BackendSetRenamed = "renamed"
)

// errNoBackendProfiles is what every method here answers when this boot
// keeps no device profile directory.
var errNoBackendProfiles = errors.New("this installation has nowhere to keep pairings, so it cannot attach to other machines")

// ListBackends returns every machine this installation has attached.
//
// Reachability is last-known and never probed: waking every attached
// laptop to answer one list would make the settings pane as slow as the
// slowest of them, and the SPA already learns the live answer from each
// carried socket.
//
//ao:scope host
//ao:route home
func (a *App) ListBackends() ([]AttachedBackend, error) {
	if a.backends == nil {
		return nil, errNoBackendProfiles
	}
	attached, err := a.backends.List()
	if err != nil {
		return nil, err
	}
	// Always a non-nil slice so the encoder emits `[]` rather than
	// `null`, and no caller needs a defensive coalesce.
	if attached == nil {
		attached = []AttachedBackend{}
	}
	return attached, nil
}

// AddBackend spends a pairing link and returns the verification number
// the owner of the far machine has to match.
//
// It returns BEFORE the pairing admits anything, and that is not a
// shortcut: the confirmation window is ten minutes
// (deviceclient.AwaitActivation), which is longer than any timeout
// between here and the page. So the wait runs on its own and the outcome
// arrives on the backend:attach channel — the same split the terminal
// ceremony makes between printing the number and waiting for it.
//
//ao:scope host
//ao:route home
func (a *App) AddBackend(pairingLink string) (BackendAttachment, error) {
	if a.backends == nil {
		return BackendAttachment{}, errNoBackendProfiles
	}
	attachment, err := a.backends.Add(a.appCtx, pairingLink)
	if err != nil {
		return BackendAttachment{}, err
	}
	a.awaitAttachment(attachment.ID)
	return attachment, nil
}

// awaitAttachment runs one confirmation wait and announces how it ended.
//
// On the App's own context so a shutdown mid-wait ends it rather than
// leaving a goroutine holding a pinned TLS transport open for ten
// minutes.
func (a *App) awaitAttachment(id string) {
	go func() {
		outcome := BackendAttachOutcome{ID: id, Attached: true}
		if err := a.backends.Await(a.appCtx, id); err != nil {
			// Logged as well as announced: this is the one error in the
			// pairing flow nobody is standing in front of, and the page
			// that asked may be long closed.
			log.Printf("app: attach backend %s: %v", id, err)
			outcome.Attached = false
			outcome.Error = err.Error()
		}
		a.emit(eventchan.BackendAttach, outcome)
	}()
}

// RemoveBackend forgets one machine. The device key survives — it names
// this DEVICE, and the far side adopts its row again by thumbprint if
// this installation ever pairs with that machine a second time.
//
//ao:scope host
//ao:route home
func (a *App) RemoveBackend(id string) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	if err := a.backends.Remove(id); err != nil {
		return err
	}
	a.emit(eventchan.BackendSetChanged, BackendSetChange{Action: BackendSetRemoved, ID: id})
	return nil
}

// RenameBackend sets what this installation calls one machine, or clears
// it when the nickname is empty. Local: nothing is sent to the machine
// being renamed, which goes on calling itself whatever it calls itself.
//
//ao:scope host
//ao:route home
func (a *App) RenameBackend(id, nickname string) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	if err := a.backends.Rename(id, nickname); err != nil {
		return err
	}
	a.emit(eventchan.BackendSetChanged, BackendSetChange{
		Action:   BackendSetRenamed,
		ID:       id,
		Nickname: nickname,
	})
	return nil
}

// AttachedBackends exposes the manager to the transport, which serves one
// carried route family per attached machine. Package-level for the same
// reason BackendIdentity is: it is boot wiring, not a bound method.
//
// Returns a nil interface rather than a typed nil when this boot keeps no
// profiles, so the transport registers no routes at all.
func AttachedBackends(a *App) *attachedbackends.Manager { return a.backends }

// SetAttachedBackends installs the manager during boot.
func SetAttachedBackends(a *App, manager *attachedbackends.Manager) { a.backends = manager }
