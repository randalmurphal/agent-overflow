package deviceclient

// The wire this package speaks, restated rather than imported.
//
// `internal/transport` declares all of these, and importing it here would
// pull an HTTP+WebSocket server into every process that only wants to
// attach to one. `internal/relaysession` has the same problem for the same
// reason and solves it the same way: restate the spellings, and pin them
// with a drift-guard test that imports the transport from _test code only
// (wire_drift_test.go).

const (
	// authPairPath spends a pairing link. The one route whose caller
	// holds nothing at all.
	authPairPath = "/auth/pair"
	// authTokenPath rotates the credential pair.
	authTokenPath = "/auth/token"
	// authTicketPath mints the single-use ticket the WebSocket upgrade
	// names its session with.
	authTicketPath = "/auth/ticket"
	// bootstrapPath is the manifest, which a paired device reaches with
	// its session credential rather than a launch token.
	bootstrapPath = "/bootstrap.json"
	// wsPath is the upgrade route.
	wsPath = "/ws"
)

const (
	// SessionCredentialHeader carries the session credential.
	SessionCredentialHeader = "X-AO-Session"
	// DeviceKeyHeader carries the per-request proof of possession.
	DeviceKeyHeader = "X-AO-Device-Key"
	// WSTicketParam carries a spent-on-first-use ticket on the upgrade
	// URL. A ticket, never a credential.
	WSTicketParam = "ticket"
)

// reasonPendingConfirmation is the one refusal code this package branches
// on, from `internal/identity`'s closed set.
//
// It is the only refusal whose remedy is on ANOTHER device and the only
// one where presenting the same credential again is expected to succeed
// shortly, which is exactly why it must not be treated as a dead session.
// Every other code means the credential will not start working on its
// own, and this package's answer to all of them is one answer: forget the
// session and say that re-pairing is the way back.
const reasonPendingConfirmation = "pending_confirmation"
