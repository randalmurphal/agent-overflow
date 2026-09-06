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
	authTokenPath        = "/auth/token"
	authTokenRecoverPath = "/auth/token/recover"
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

// Pending confirmation is the renewal refusal whose remedy is on the host.
const reasonPendingConfirmation = "pending_confirmation"
