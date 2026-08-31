package app

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/network"
	"agent-overflow/internal/store"
)

// The device-access surface (docs/specs/remote-access.md §4-§6): what the
// settings pane reads to show which devices hold credentials on this
// backend, and the calls that add one or take one away.
//
// Everything here is adaptation over internal/identity, on the same terms
// as app_identity.go: no policy decision lives in this file, because a
// decision made here is one the session core could not enforce for a
// caller that reached it another way. What this surface DOES own is the
// wire shape (app_access_types.go) and the two refusals that are facts
// about this process rather than about a row: the local page channel is
// not a device a person may revoke, and a device class this backend does
// not pair is not one a mint may name.
//
// All of these are LocalOnlyMethods (CategoryDeviceAccess). Minting a
// pairing link issues a credential and revoking one withdraws every
// credential a device holds; neither belongs to a LAN-attached caller,
// and the audit read is the map for both.

// accessAuditLimit is how much credential history one overview carries.
// Fifty rows is a screenful of scrollback and covers the exchange a
// person is usually looking at (a mint, a redemption, a confirmation, the
// refusals around them) without putting the whole table on the wire; the
// full log is bounded separately at maxAuthAuditRows.
const accessAuditLimit = 50

// pairingFragmentPrefix is what the redeeming page looks for in the URL
// fragment. Named rather than spelled twice: the frontend reads the same
// prefix, and a mismatch is a link that silently does nothing.
const pairingFragmentPrefix = "#pair="

// GetAccessOverview returns the device, pairing, and audit lists in one
// read.
//
//ao:scope access:admin
func (a *App) GetAccessOverview() (AccessOverview, error) {
	state, err := a.accessState()
	if err != nil {
		return AccessOverview{}, err
	}
	devices, err := a.store.ListDevicesForUser(state.owner.ID)
	if err != nil {
		return AccessOverview{}, fmt.Errorf("access: list devices: %w", err)
	}
	now := state.sessions.Now()

	out := AccessOverview{Devices: make([]AccessDevice, 0, len(devices))}
	for _, device := range devices {
		view := AccessDevice{
			ID:           device.ID,
			Label:        device.Label,
			Class:        device.Class,
			Platform:     device.Platform,
			Channel:      device.Channel,
			CreatedAtMs:  device.CreatedAt,
			LastSeenAtMs: device.LastSeenAt,
			RevokedAtMs:  device.RevokedAt,
		}
		sessions, err := a.store.ListSessionsForDevice(device.ID)
		if err != nil {
			return AccessOverview{}, fmt.Errorf("access: list sessions for %s: %w", device.ID, err)
		}
		for _, session := range sessions {
			if !presentableSession(session, now) {
				continue
			}
			view.Sessions = append(view.Sessions, AccessSession{
				ID:                   session.ID,
				Binding:              session.BindingClass,
				AwaitingConfirmation: session.AwaitingConfirmation(),
				CreatedAtMs:          session.CreatedAt,
				ActivatedAtMs:        session.ActivatedAt,
				LastUsedAtMs:         session.LastSeenAt,
				ExpiresAtMs:          session.ExpiresAt,
				Connections:          a.sessionConnCount(session.ID),
			})
		}
		out.Devices = append(out.Devices, view)
	}

	pending, err := a.pendingPairings(state, now)
	if err != nil {
		return AccessOverview{}, err
	}
	out.PendingPairings = pending

	audit, err := a.store.ListRecentAuthAudit(accessAuditLimit)
	if err != nil {
		return AccessOverview{}, fmt.Errorf("access: read the credential log: %w", err)
	}
	for _, entry := range audit {
		out.Audit = append(out.Audit, AccessAuditEntry{
			AtMs:      entry.At,
			Event:     entry.Event,
			Outcome:   entry.Outcome,
			DeviceID:  entry.DeviceID,
			SessionID: entry.SessionID,
			Detail:    entry.Detail,
			Peer:      entry.Peer,
		})
	}
	return out, nil
}

// MintDevicePairing issues a pairing link for a new device of the named
// class, and returns the URL that device should open.
//
// The CLASS is decided here and the redeeming device never names its own
// (identity.PairingRequest says why). `backend-peer` is refused: enrolling
// another backend is §8's federation flow, with its own trust decisions,
// and admitting one through the device-pairing surface would grant it the
// posture of an owner's own device.
//
//ao:scope access:admin
//ao:stepup
func (a *App) MintDevicePairing(deviceClass string) (PairingInvite, error) {
	state, err := a.accessState()
	if err != nil {
		return PairingInvite{}, err
	}
	class := identity.DeviceClass(deviceClass)
	if !class.Valid() {
		return PairingInvite{}, fmt.Errorf("access: %q is not a declared device class", deviceClass)
	}
	if class == identity.DeviceBackendPeer {
		return PairingInvite{}, fmt.Errorf(
			"access: a peer backend is enrolled through the federation flow, not device pairing")
	}

	pageURL, endpoint, err := a.pairingPageURL()
	if err != nil {
		return PairingInvite{}, err
	}
	backendID, _ := a.backendIdentity()
	if backendID == "" {
		// Every credential this link leads to is MAC'd over the backend
		// id, and a payload naming none would leave the device unable to
		// say which backend offered to pair. Refuse rather than mint a
		// link that resolves to an anonymous backend.
		return PairingInvite{}, fmt.Errorf("access: the backend identity is not available yet")
	}

	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:      state.owner.ID,
		DeviceClass: class,
		// Device-bound on every class this surface mints. The link's whole
		// point is a device that holds its own key; `loopback-only` is the
		// local channel's posture and is never handed to a paired device.
		BindingClass: identity.BindingDeviceBound,
		// The full declared set, matching what the local channel grants
		// (identity.EnsureLocalChannelSession). Narrowing per device is a
		// real feature (§5, "narrowing is offered per-device") and is not
		// this call: what each scope PERMITS is phase 3, so a narrower
		// grant here would describe a restriction nothing enforces.
		Scopes: identity.Scopes,
	})
	if err != nil {
		return PairingInvite{}, err
	}

	payload, err := identity.PairingPayload{
		Version:     identity.PairingPayloadVersion,
		BackendID:   backendID,
		BackendName: backendDisplayName(),
		Endpoint:    endpoint,
		Token:       link.Token,
	}.Encode()
	if err != nil {
		return PairingInvite{}, err
	}
	return PairingInvite{
		LinkID:      link.Link.ID,
		URL:         pageURL + pairingFragmentPrefix + payload,
		ExpiresAtMs: link.Link.ExpiresAt,
	}, nil
}

// DevicePairingStatus reads one link, for the surface polling while it
// waits.
//
//ao:scope access:admin
func (a *App) DevicePairingStatus(linkID string) (PairingStatusView, error) {
	state, err := a.accessState()
	if err != nil {
		return PairingStatusView{}, err
	}
	link, number, err := state.sessions.PairingStatus(linkID)
	if err != nil {
		return PairingStatusView{}, err
	}
	return PairingStatusView{
		LinkID:             link.ID,
		State:              pairingState(link, state.sessions.Now()),
		VerificationNumber: number,
		DeviceLabel:        a.pairedDeviceLabel(link),
		ExpiresAtMs:        pairingDeadline(link),
	}, nil
}

// ConfirmDevicePairing activates the session a redemption created, after
// the owner has matched the verification number.
//
//ao:scope access:admin
func (a *App) ConfirmDevicePairing(linkID string) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	_, err = state.sessions.ConfirmPairing(linkID)
	return err
}

// CancelDevicePairing refuses a link and revokes whatever a redemption
// already created — the number did not match, or the link was minted by
// mistake.
//
//ao:scope access:admin
func (a *App) CancelDevicePairing(linkID string) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	_, err = state.sessions.CancelPairing(linkID)
	return err
}

// RevokeAccessDevice ends a device and every session it holds, then drops
// its persisted UI state (§6: revoking a device drops its state).
//
// The local page channel is refused. That row is not a device somebody
// paired: it is this backend's own page channel, which the embedded
// webview, the WSL relay and the `--connect` stub all present. Revoking
// it would sign the host's own window out, and identity refuses to
// re-mint around a revoked channel device by design — so the refusal
// belongs in front of the call, where it can say what happened.
//
//ao:scope access:admin
func (a *App) RevokeAccessDevice(deviceID string) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	device, err := a.store.GetDevice(deviceID)
	if err != nil {
		return fmt.Errorf("access: read device %s: %w", deviceID, err)
	}
	if device.Channel == identity.LocalChannel {
		return fmt.Errorf(
			"access: %q is this app's own page channel, not a paired device; revoking it would sign this window out",
			device.Label)
	}
	if _, err := state.sessions.RevokeDevice(deviceID); err != nil {
		return err
	}
	// After the revocation, never before: the state belongs to a device
	// that still holds credentials until RevokeDevice returns, and
	// dropping it first would clear a working device's preferences if the
	// revocation then failed.
	if _, err := a.store.DeleteUIStateScope("device:" + deviceID); err != nil {
		// The device is revoked, which is the part that had to happen.
		// Reporting an error now would invite a retry that finds nothing
		// left to revoke, so say it in the log and answer success.
		log.Printf("access: drop ui state for revoked device %s: %v", deviceID, err)
	}
	return nil
}

// RestoreAccessDevice re-admits a revoked device's key to pairing — the
// remedy RedeemPairing's revoked-key refusal names. No credential moves:
// the device still redeems a fresh link and passes the number check.
//
//ao:scope access:admin
func (a *App) RestoreAccessDevice(deviceID string) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	if _, err := state.sessions.RestoreDevice(deviceID); err != nil {
		return err
	}
	return nil
}

// RevokeAccessSession ends one session, leaving its device paired.
//
// Refused for the local page channel on the same grounds as
// RevokeAccessDevice, resolved through the session's device row: the
// channel's session is re-minted at boot, so revoking it mid-run would
// close the host's own window until a restart.
//
//ao:scope access:admin
func (a *App) RevokeAccessSession(sessionID string) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	session, err := a.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("access: read session %s: %w", sessionID, err)
	}
	device, err := a.store.GetDevice(session.DeviceID)
	if err != nil {
		return fmt.Errorf("access: read the session's device: %w", err)
	}
	if device.Channel == identity.LocalChannel {
		return fmt.Errorf(
			"access: that session belongs to this app's own page channel; revoking it would sign this window out")
	}
	_, err = state.sessions.RevokeSession(sessionID)
	return err
}

// accessState resolves the session core and the owner account, with one
// error every method in this file reports the same way. Identity not
// being wired is a state, not a fault (app_identity.go), but it is one
// this surface cannot answer anything from.
func (a *App) accessState() (*identityState, error) {
	if a.store == nil {
		return nil, fmt.Errorf("access: store unavailable")
	}
	state := a.identityState()
	if state == nil {
		return nil, fmt.Errorf("access: the session core is not running; device access is unavailable")
	}
	return state, nil
}

// sessionConnCount asks the transport how many sockets carry a session.
// Zero before the transport exists, which is the honest answer for an App
// that is not serving.
func (a *App) sessionConnCount(sessionID string) int {
	srv := a.transportServer.Load()
	if srv == nil {
		return 0
	}
	return srv.SessionConns().CountForSession(sessionID)
}

// presentableSession decides which of a device's sessions the overview
// carries: the ones a person can act on. A live session is one; a session
// awaiting confirmation is the other, because it is the one the owner is
// being asked about. Expired and revoked rows are history, and the audit
// log already holds them.
func presentableSession(session store.Session, now int64) bool {
	if session.Live(now) {
		return true
	}
	return session.AwaitingConfirmation() && session.ExpiresAt > now
}

// pendingPairings lists the links the owner can still act on.
func (a *App) pendingPairings(state *identityState, now int64) ([]PendingPairing, error) {
	links, err := a.store.ListPairingLinksForUser(state.owner.ID, accessAuditLimit)
	if err != nil {
		return nil, fmt.Errorf("access: list pairing links: %w", err)
	}
	var out []PendingPairing
	for _, link := range links {
		if link.Settled() || pairingDeadline(link) <= now {
			continue
		}
		view := PendingPairing{
			LinkID:      link.ID,
			CreatedAtMs: link.CreatedAt,
			ExpiresAtMs: pairingDeadline(link),
			Redeemed:    link.Redeemed(),
		}
		if link.Redeemed() {
			number, err := state.sessions.VerificationNumber(link.ID, link.KeyThumbprint)
			if err != nil {
				return nil, fmt.Errorf("access: derive verification number for %s: %w", link.ID, err)
			}
			view.VerificationNumber = number
			view.DeviceLabel = a.pairedDeviceLabel(link)
		}
		out = append(out, view)
	}
	return out, nil
}

// pairedDeviceLabel names the device that redeemed a link, or "" when
// nothing has. A read failure is not an error: the label is presentation,
// and losing it must not fail a status poll the owner is waiting on.
func (a *App) pairedDeviceLabel(link store.PairingLink) string {
	if link.DeviceID == "" {
		return ""
	}
	device, err := a.store.GetDevice(link.DeviceID)
	if err != nil {
		log.Printf("access: read the redeeming device for %s: %v", link.ID, err)
		return ""
	}
	return device.Label
}

// pairingState names where a link stands.
//
// The order is the contract. Settled first, because confirmed and
// canceled are terminal and outlive every window. Redeemed BEFORE
// expired, because a redeemed link deliberately outlives its own
// five-minute window: redemption mints the session with the confirmation
// window (identity.PairingConfirmWindow), and reporting "expired" while
// the owner still has minutes to match the number would take away the
// step the exchange is waiting on.
func pairingState(link store.PairingLink, now int64) string {
	switch {
	case link.ConfirmedAt != 0:
		return pairingStateConfirmed
	case link.CanceledAt != 0:
		return pairingStateCanceled
	case link.Redeemed():
		if pairingDeadline(link) <= now {
			return pairingStateExpired
		}
		return pairingStateRedeemed
	case link.ExpiresAt <= now:
		return pairingStateExpired
	default:
		return pairingStatePending
	}
}

// pairingDeadline is when a link stops being actionable, which is not
// always the row's own expiry: once a device has redeemed, the deadline
// is the owner's confirmation window. One field with one meaning beats an
// `expiresAt` that reads as long past while the exchange is still live.
func pairingDeadline(link store.PairingLink) int64 {
	if link.Redeemed() {
		return link.RedeemedAt + identity.PairingConfirmWindow.Milliseconds()
	}
	return link.ExpiresAt
}

// pairingPageURL assembles the page URL a pairing link points at, and the
// bare origin the payload names as its redemption endpoint.
//
// Host rule: the LAN share URL when the listener is bound wide, the
// loopback URL otherwise — network.FromServer is the same primitive
// GetNetworkSettings answers with, so the address on a pairing link and
// the address in the share panel can never disagree. Both carry a freshly
// minted one-time page ticket, which is what lets a device holding no
// credential load the page at all.
//
// No `cid` parameter, deliberately: that is the local install's durable
// UI-state identity, and stamping it on a link for somebody else's phone
// would point that phone at this machine's bucket.
func (a *App) pairingPageURL() (pageURL, endpoint string, err error) {
	srv := a.transportServer.Load()
	if srv == nil {
		return "", "", fmt.Errorf("access: transport server unavailable")
	}
	settings := network.FromServer(srv, a.currentSettings().Network.BindAll)
	if settings.URL == "" {
		return "", "", fmt.Errorf("access: the transport has no page URL to pair against yet")
	}
	parsed, err := url.Parse(settings.URL)
	if err != nil {
		return "", "", fmt.Errorf("access: parse the page URL: %w", err)
	}
	return settings.URL, parsed.Scheme + "://" + parsed.Host, nil
}

// backendDisplayName is the name a pairing device shows while it decides
// whether to trust this offer. The hostname, because that is what a
// person recognises about the machine they are pairing to.
//
// Convenience only: the payload documents that it grants nothing and is
// never matched against anything, so an unreadable hostname is an empty
// field rather than a failure.
func backendDisplayName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}
