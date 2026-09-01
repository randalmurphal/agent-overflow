package app

// The wire shapes of the device-access surface (app_access.go).
//
// Flat structs with json tags, millisecond epochs, and `omitempty` where
// a zero value means absent — the same additive-only discipline every
// other shape on this wire keeps. A field may be appended; none may
// change meaning, because a client built against an older build still
// reads the ones it knows.
//
// Separate from the methods so the shape a frontend renders can be read
// on its own, without the adaptation around it.

// AccessOverview is the whole settings surface in one call. One call
// because the three lists are read together and shown together, and three
// round trips would let the device list disagree with the audit line that
// explains it.
type AccessOverview struct {
	// Devices is every device row of the owner account, revoked ones
	// included: the list is also the record of what was removed.
	Devices []AccessDevice `json:"devices"`
	// PendingPairings is every link still waiting on somebody — to be
	// redeemed, or to have its verification number confirmed.
	PendingPairings []PendingPairing `json:"pendingPairings,omitempty"`
	// Audit is the most recent credential events, newest first.
	Audit []AccessAuditEntry `json:"audit,omitempty"`
}

// AccessDevice is one client instance that holds, or held, credentials.
type AccessDevice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Class    string `json:"class"`
	Platform string `json:"platform,omitempty"`
	// Channel is non-empty only for a device this backend minted for
	// ITSELF — today just the local page channel. The surface reads it to
	// present that row differently and to hide a revoke control that
	// would refuse anyway.
	Channel      string `json:"channel,omitempty"`
	CreatedAtMs  int64  `json:"createdAtMs"`
	LastSeenAtMs int64  `json:"lastSeenAtMs,omitempty"`
	RevokedAtMs  int64  `json:"revokedAtMs,omitempty"`
	// Sessions carries the live and awaiting-confirmation sessions, plus
	// any that outlived their device's revocation (SurvivedRevocation).
	// A device's expired and ordinarily revoked sessions are history the
	// audit log already holds, and shipping them would grow this list
	// without bound for a device that has reconnected for months.
	Sessions []AccessSession `json:"sessions,omitempty"`
}

// AccessSession is one credential a device currently holds.
type AccessSession struct {
	ID      string `json:"id"`
	Binding string `json:"binding"`
	// AwaitingConfirmation is true for a session a redemption minted that
	// the owner has not confirmed. It exists and admits nothing.
	AwaitingConfirmation bool  `json:"awaitingConfirmation,omitempty"`
	CreatedAtMs          int64 `json:"createdAtMs"`
	ActivatedAtMs        int64 `json:"activatedAtMs,omitempty"`
	LastUsedAtMs         int64 `json:"lastUsedAtMs,omitempty"`
	ExpiresAtMs          int64 `json:"expiresAtMs"`
	// Connections is how many sockets are carrying this session right
	// now, read from the transport's live-session registry. Zero for a
	// session nobody is attached on, which is a normal answer.
	Connections int `json:"connections,omitempty"`
	// Scopes is the grant set this session was minted with, verbatim.
	// Carried rather than reduced to a label, because what "view only"
	// MEANS is the frontend's own definition (`transport/scopes.ts`
	// isViewOnlyGrantSet, which the page already applies to itself) and
	// two copies of it would agree only until one moved. Empty is a real
	// answer for a session granted nothing.
	Scopes []string `json:"scopes,omitempty"`
	// SurvivedRevocation marks the state that should not exist: this
	// credential was NOT withdrawn while its device was. Revoking a
	// device revokes its sessions in one transaction
	// (store.RevokeDevice), so a true here means that invariant broke,
	// and the surface renders it as the anomaly it is rather than as
	// another row (docs/specs/remote-access.md §2).
	SurvivedRevocation bool `json:"survivedRevocation,omitempty"`
}

// PendingPairing is one link the owner can still act on.
type PendingPairing struct {
	LinkID      string `json:"linkId"`
	CreatedAtMs int64  `json:"createdAtMs"`
	// ExpiresAtMs is when this pairing stops being actionable — see
	// pairingDeadline, which is not always the link row's own expiry.
	ExpiresAtMs int64 `json:"expiresAtMs"`
	// Redeemed is true once a device has presented the token and its key.
	// The two fields below are populated only then.
	Redeemed           bool   `json:"redeemed,omitempty"`
	DeviceLabel        string `json:"deviceLabel,omitempty"`
	VerificationNumber string `json:"verificationNumber,omitempty"`
}

// AccessAuditEntry is one row of the credential log.
type AccessAuditEntry struct {
	AtMs      int64  `json:"atMs"`
	Event     string `json:"event"`
	Outcome   string `json:"outcome"`
	DeviceID  string `json:"deviceId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Peer      string `json:"peer,omitempty"`
}

// PairingInvite is what a fresh mint hands the surface: the id to poll on
// and the URL to show as a link or a QR code.
type PairingInvite struct {
	LinkID string `json:"linkId"`
	// URL is a full page URL — a spendable one-time page ticket included,
	// because the redeeming device holds no credential and could not load
	// the page otherwise — with the pairing payload in the FRAGMENT. A
	// fragment is never sent to a server, never written to an access log,
	// and never lands in a Referer header.
	URL         string `json:"url"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

// PairingStatusView is one link's state, for the surface polling while it
// waits for a device to redeem.
type PairingStatusView struct {
	LinkID string `json:"linkId"`
	// State is one of: pending, redeemed, confirmed, canceled, expired.
	State string `json:"state"`
	// VerificationNumber and DeviceLabel are populated once a device has
	// redeemed. The number is what the owner compares against the one the
	// device is showing before confirming.
	VerificationNumber string `json:"verificationNumber,omitempty"`
	DeviceLabel        string `json:"deviceLabel,omitempty"`
	ExpiresAtMs        int64  `json:"expiresAtMs"`
}

// Redeemed reports that a device has claimed the link and is showing the
// number the owner has to match. It is the one state a waiting surface
// ACTS on rather than keeps waiting through.
func (v PairingStatusView) Redeemed() bool { return v.State == pairingStateRedeemed }

// Settled reports that the exchange is over, whichever way it went, so a
// poll loop can stop.
func (v PairingStatusView) Settled() bool {
	switch v.State {
	case pairingStateConfirmed, pairingStateCanceled, pairingStateExpired:
		return true
	}
	return false
}

// Confirmed reports the one settled state that actually enrolled the
// device. Canceled and expired are the other two, and a caller that
// treated "settled" as "done, it worked" would tell an operator their
// phone was paired when the link had run out.
func (v PairingStatusView) Confirmed() bool { return v.State == pairingStateConfirmed }

// The three predicates above exist so a caller outside this package can
// branch on a link's state without restating these strings. The constants
// stay unexported deliberately — the vocabulary is this surface's, and a
// second spelling of "redeemed" somewhere else is a bug nothing would
// catch. Methods on the DTO rather than on App, so they never become wire
// RPCs: internal/transport/methodgen scans one receiver (*App) and these
// are invisible to it by construction.

// DeviceRevocationResult is what one RevokeAccessDevice actually did.
//
// It exists because "revoked, 2 sessions ended, 1 connection closed" and
// "already revoked, nothing was live" are different answers and the person
// who just lost a phone needs to be told which one they got. The call
// reported success uniformly until wave 7c, so a second revoke that swept
// nothing looked exactly like the first one that swept everything —
// which is how a device that kept access went unnoticed
// (docs/specs/remote-access.md §2).
type DeviceRevocationResult struct {
	// DeviceMoved is false when the device row was already revoked. Not a
	// failure: re-revoking is a legitimate thing to do, and it still
	// re-sweeps and still closes sockets.
	DeviceMoved bool `json:"deviceMoved"`
	// SessionsEnded is how many un-revoked credentials this call ended.
	SessionsEnded int `json:"sessionsEnded"`
	// ConnectionsClosed is how many live sockets it force-closed. It can
	// exceed SessionsEnded (one session, several tabs) and it can be
	// non-zero when SessionsEnded is not, which is the case worth seeing:
	// a socket that survived an earlier revocation.
	ConnectionsClosed int `json:"connectionsClosed"`
}

// Pairing states. Strings on the wire rather than an integer, because the
// surface renders them and the audit log spells the same words.
const (
	pairingStatePending   = "pending"
	pairingStateRedeemed  = "redeemed"
	pairingStateConfirmed = "confirmed"
	pairingStateCanceled  = "canceled"
	pairingStateExpired   = "expired"
)
