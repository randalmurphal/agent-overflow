package attachedbackends

// SetChange is the frame the backend:set-changed channel carries: every
// mutation of the attached-machine SET that is not the end of a pairing
// ceremony. Two desktops emit it — internal/app for the one with a backend
// and internal/frontendclient for the frontend-only one — and both take it
// from here, so the frontend mirrors ONE shape
// (frontend/src/lib/stores/systems.svelte.ts BackendSetChangeEvent).
// internal/app's TestBackendSetChangeVocabularyMatchesTheFrontend pins the
// action set against that mirror and refuses an emit that spells one by hand.
//
// Its own channel rather than a second meaning on backend:attach, because
// the two answer different questions — "how did the pairing I started end"
// and "the list moved" — and a receiver that conflated them would retire a
// pending row on a rename. Two pages open on this host, and the same page
// after a reload, converge on it.
type SetChange struct {
	Action SetAction `json:"action"`
	// ID names the machine for every action but SetMembership, which says
	// only that the set is worth re-reading.
	ID string `json:"id"`
	// Nickname is what this installation now calls the machine, empty when
	// the name was cleared or the row was removed.
	Nickname string `json:"nickname,omitempty"`
	// Reason says who ended a SetRemoved machine's pairing. Absent for the
	// removal this installation made itself, which is every removal there
	// was before the field existed, so an older page reads the frame as it
	// always did.
	Reason RemovalReason `json:"reason,omitempty"`
}

// RemovalReason is who ended a removed machine's pairing.
type RemovalReason string

const (
	// RemovedHere: this installation forgot the machine (Remove). The zero
	// value, and omitted from the frame.
	RemovedHere RemovalReason = ""
	// RemovedByComputer: the far machine refused this installation's
	// renewal with a verdict that ends the session — its owner revoked this
	// device — and the client forgot its own profile. The page says so,
	// because nothing else will: this installation asked for nothing.
	RemovedByComputer RemovalReason = "ended-by-computer"
)

// SetAction is what happened to the set.
type SetAction string

const (
	// SetRemoved: the machine's profile is gone.
	SetRemoved SetAction = "removed"
	// SetRenamed: this installation's nickname for the machine changed.
	SetRenamed SetAction = "renamed"
	// SetDeviceNameSync: the far machine's copy of this installation's own
	// display name was synchronized, or that failed; the listing's per-row
	// name state is worth re-reading.
	SetDeviceNameSync SetAction = "device-name-sync"
	// SetMembership: own-device reconciliation added, pruned or errored a
	// profile; the whole listing is worth re-reading.
	SetMembership SetAction = "membership"
)

// SetChanged registers the one observer of set mutations. Every emit of
// backend:set-changed comes through it, whichever desktop hosts the
// manager, so a mutation the manager makes on its own — a reconciler
// prune, a name synchronization — reaches the page the same way a removal
// the page asked for does. Register before serving.
func (m *Manager) SetChanged(changed func(SetChange)) {
	m.mu.Lock()
	m.changed = changed
	m.mu.Unlock()
}

// notifyChanged runs the observer outside every manager lock: the
// observer emits, and an emit may re-enter the manager through a listing.
func (m *Manager) notifyChanged(change SetChange) {
	m.mu.Lock()
	changed := m.changed
	m.mu.Unlock()
	if changed != nil {
		changed(change)
	}
}
