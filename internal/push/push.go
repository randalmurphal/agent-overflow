// Package push carries a notification to a phone that is not connected.
//
// It is the outbound half of the pipe `internal/notify` maps
// (docs/specs/remote-access.md §9, "Push"). An attached client raises its
// own OS notification off the `notification:send` channel; a phone with no
// socket cannot, so the same `notify.Send` is handed here and the platform
// wakes the app instead. One mapping, two deliveries.
//
// THREE PROPERTIES HOLD THIS PACKAGE TOGETHER, and every one of them is a
// design decision rather than an implementation detail:
//
//   - DATA-ONLY, ALWAYS. A message carries no platform-composed
//     notification block, so the phone's own service renders the tray entry
//     and Google never composes one. That is what makes a RETRACTION
//     expressible at all — withdrawing a notification is a message like any
//     other — and it is what keeps the rendering decision on the device.
//   - THE PAYLOAD IS A FIXED PHRASE AND A MACHINE NAME. §9's redaction rule
//     is that a push transits Google, so what a person reads on a lock
//     screen says only which of the moments happened and which machine it
//     happened on. The thread title the DESKTOP notification carries does
//     not cross this line; the app fetches it after the tap, over the
//     paired session. `MessageFor` is the only constructor, so there is no
//     second way to fill the map.
//   - ONE ERROR IS ACTIONABLE. `ErrTokenGone` says the registration this
//     message named will never be valid again, which is the caller's cue to
//     drop the row. Everything else is a failure to REACH the platform and
//     is retried by the next moment, never by deleting state.
//
// Nothing here knows about SQLite, settings or the App: a Sender takes a
// prepared message and answers. `internal/app`'s fan-out owns which devices
// get one and whose preferences gate it.
package push

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/notify"
)

// Sender delivers one prepared message to one device's registration token.
//
// An interface with one method because the seam has exactly one job and two
// realizations that must not diverge: the FCM sender below, and the
// harness's recorder. A later "the owner's home backend relays for a
// friend's backend" (§18 item 1) is a third implementation of this method
// and nothing above it changes — which is the whole reason the fan-out
// talks to a Sender rather than to FCM.
type Sender interface {
	Send(ctx context.Context, message Message) error
}

// ErrTokenGone reports a registration token the platform will never accept
// again: the app was uninstalled, its data was cleared, or the registration
// aged out.
//
// It is the ONE error a caller acts on, and the action is to delete the row
// rather than to retry. Every other failure — a refused credential, a 5xx,
// a phone with no network — describes this attempt and not the token, so
// treating them alike would have one bad afternoon unregister every phone
// the owner has.
var ErrTokenGone = errors.New("push: this registration token is no longer valid")

// Message is one delivery: which device, what the tray keys it by, and the
// flat string map the phone's service reads.
//
// Tag is the moment's stable id (`notify.Send.ID`). It rides as the
// platform's collapse key, so a send that is still queued for an offline
// phone is REPLACED by the next send about the same moment rather than
// delivered behind it — the offline half of the same replace-in-place rule
// `internal/notify` states for the tray. The device-side tray tag is
// composed from the `backend` and `id` data keys (`TrayTag`), because a
// collapse key is scoped to one sender's project while a tray is shared by
// every backend the phone is paired with.
type Message struct {
	Token string
	Tag   string
	Data  map[string]string
}

// The data keys a message carries, spelled once here and mirrored by the
// phone's renderer (`mobile/android/.../push/TrayNotifier.java`). Two
// halves of one wire contract, so a rename that missed the other end is a
// notification the phone drops silently.
const (
	// KeyID is the moment's stable id, and half of the tray tag: what a
	// later state change replaces and what a retraction cancels.
	KeyID = "id"
	// KeyBackend is the sending backend's identity (`store.Identity.BackendID`),
	// the other half of the tray tag. On EVERY message, retractions included.
	//
	// A phone is paired with several backends and notification ids are not
	// unique across them — `provider-auth:claude` is the same string on every
	// machine the owner runs — so a tag of the id alone lets one machine's
	// sign-out notice silently replace another's. And a phone whose socket is
	// still up while the OS has the app in the background is told about one
	// moment TWICE, once on the wire and once through here; the two paths
	// share this tag so the second one replaces the first rather than
	// stacking beside it. The socket path reads the same identity off the
	// frame's origin, so the two spell it from one source.
	KeyBackend = "backend"
	// KeyKind is the `notify.Kind` this moment is. The gate that reads it
	// runs on the backend, before the send; it rides anyway because the
	// renderer groups by it and a later per-kind channel needs no new key.
	KeyKind = "kind"
	// KeyRetract is present, and equal to RetractValue, only on a
	// withdrawal. Absent is the common case, and absent is what an older
	// renderer reads as "present this".
	KeyRetract = "retract"
	// KeyTitle is the kind's FIXED phrase (`notify.KindPhrase`). Never the
	// thread's title: that is the field §9's redaction rule is about.
	KeyTitle = "title"
	// KeyBody is this backend's display name and nothing else — which
	// machine is asking, so an owner with two desktops knows which one to
	// walk to.
	KeyBody = "body"
	// KeyTarget is the JSON encoding of `notify.Target`, carried whole
	// rather than flattened into sibling keys.
	//
	// ONE KEY, and the reason is a collision that would otherwise be
	// silent: `Target`'s own JSON spells its route `kind`, and so does the
	// notification kind above. Flattening the two into one map would make
	// one overwrite the other, differently depending on iteration order.
	// Carrying the target as its own document also keeps the phone's
	// renderer from having to know a single one of its field names — it
	// hands the string on, and the SPA parses it with the same
	// `parseNotificationTarget` every other activation goes through.
	KeyTarget = "target"
)

// RetractValue is what KeyRetract holds when a message withdraws one.
// A string because a platform data map carries strings only.
const RetractValue = "1"

// MessageFor builds the delivery for one validated send to one token.
//
// The only constructor, so the redaction rule is a property of the type
// rather than a habit: a caller cannot add a key, and the two fields a
// person reads are a fixed phrase and the backend's name.
//
// backendID is this backend's identity and is REQUIRED: it is half of the
// tray tag, and a message without it would post under a tag no retraction
// from this backend could ever find. backendName is
// `appidentity.HostDisplayName` at the call site; empty is legal there and
// reads as "this machine did not say", which is better than inventing one.
func MessageFor(send notify.Send, token, backendID, backendName string) (Message, error) {
	if err := notify.ValidateSend(send); err != nil {
		return Message{}, err
	}
	if token == "" {
		return Message{}, errors.New("push: a message needs a registration token")
	}
	if backendID == "" {
		return Message{}, errors.New("push: a message needs the sending backend's identity")
	}
	data := map[string]string{
		KeyID:      send.ID,
		KeyBackend: backendID,
		KeyKind:    string(send.Kind),
	}
	if send.Retract {
		// Held to the same narrower contract `notify.ValidateSend` holds a
		// retraction to: an id and a kind, nothing to render, nowhere to go.
		// The backend rides anyway, because it is part of what cancel finds.
		data[KeyRetract] = RetractValue
		return Message{Token: token, Tag: send.ID, Data: data}, nil
	}
	target, err := notify.TargetJSON(send.Target)
	if err != nil {
		return Message{}, fmt.Errorf("push: encode notification target: %w", err)
	}
	data[KeyTitle] = notify.KindPhrase(send.Kind)
	data[KeyBody] = backendName
	data[KeyTarget] = target
	return Message{Token: token, Tag: send.ID, Data: data}, nil
}

// TrayTag is the tag a phone's tray keys one moment by: the backend's
// identity, a separator, the send id.
//
// Spelled here so the Go tests pin the rule the two device-side readers
// mirror — `TrayNotifier.tagFor` for the pushed message and
// `stores/pushPresenter.svelte.ts`'s `pushTag` for the socket path. Nothing
// on the backend keys by it; it exists so the contract has one home.
func TrayTag(backendID, id string) string {
	return backendID + "|" + id
}
