package transport

import "net/url"

// ClientIdentity names the screen an RPC came from. It is the transport's
// answer to "who did this?" for bound methods that need to attribute a write
// or suppress a client's echo of its own change.
//
// Two ids, because they answer two different questions and neither substitutes
// for the other:
//
//   - DeviceID is durable and per browser profile / installation. It survives
//     reloads and reconnects, and it is what a future "edited on <device>"
//     affordance would render. It is NOT unique per screen: two tabs of one
//     browser share it, because it is stored per origin.
//   - ConnectionID is minted fresh per page load and lives only in memory. Two
//     tabs have two of them, which is what makes it the correct key for
//     "was this frame my own echo?". It is not meaningful to a person and must
//     never be rendered.
//
// A client that supplies neither is anonymous, which is a normal state: the
// harness, the e2e suite, and every backend-initiated write (a saga restoring
// a draft, a queue dispatch consuming one) have no screen behind them. Frames
// they produce carry no identity and every client applies them.
type ClientIdentity struct {
	DeviceID     string `json:"deviceId,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
}

// IsZero reports whether no identity was supplied.
func (c ClientIdentity) IsZero() bool {
	return c.DeviceID == "" && c.ConnectionID == ""
}

// Query parameter names on the WebSocket upgrade URL. The identity rides the
// URL rather than a handshake frame because it must be known before the first
// RPC is dispatched: a draft saved in the window before a handshake landed
// would carry no attribution and would echo back into the composer that typed
// it.
const (
	deviceIDParam     = "did"
	connectionIDParam = "conn"
)

// ParseClientIdentity reads the identity a client declared on its upgrade URL.
//
// Both values are peer-supplied and are echoed to other attached clients on
// event frames, so each is bounded to an opaque id shape (8..64 chars of
// [A-Za-z0-9-], matching validClientID in app_uistate.go, which accepts what
// both sides mint: Go's uuid.NewString and the browser's crypto.randomUUID).
// Anything else reads as no identity rather than as an error: a client that
// spells its id wrongly should lose attribution, not its connection.
func ParseClientIdentity(query url.Values) ClientIdentity {
	return ClientIdentity{
		DeviceID:     validClientIdentityID(query.Get(deviceIDParam)),
		ConnectionID: validClientIdentityID(query.Get(connectionIDParam)),
	}
}

func validClientIdentityID(id string) string {
	if len(id) < 8 || len(id) > 64 {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return ""
		}
	}
	return id
}
