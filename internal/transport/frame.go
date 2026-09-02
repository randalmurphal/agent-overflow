package transport

import (
	"encoding/json"
	"errors"
	"slices"
)

// FrameType discriminates the wire frames. Encoded as the "type" field
// in every frame so the decoder can route without sniffing other fields.
const (
	frameTypeRPC       = "rpc"
	frameTypeEvent     = "event"
	frameTypeReplay    = "replay"
	frameTypeSubscribe = "subscribe"
	frameTypeWatch     = "watch"
	frameTypeLease     = "lease"
	frameTypeBatch     = "batch"
	frameTypePing      = "ping"
	frameTypeHello     = "hello"
)

// ProtocolVersion is the wire dialect this build speaks, stated in the
// hello frame on every connection.
//
// It is a FACT, not a gate. Nothing on either side compares it to decide
// whether to proceed: features negotiate through capability flags, so a
// client asks "does this backend have X" and degrades on the answer
// instead of guessing from a number
// (docs/specs/remote-access.md §9, compatibility policy). The version is
// here for logs, bug reports, and the day a change is genuinely not
// expressible as a flag — at which point the decision to gate on it is
// made deliberately, with this comment as the record that no such
// decision has been made yet.
//
// Bump it only for a change that alters what an EXISTING frame or field
// means. Adding a frame type, a field, or a channel is additive and does
// not move it: additive-only is the discipline that makes the swap window
// (an old bundle live against a just-updated backend) safe.
const ProtocolVersion = 1

// serverCapabilities is the set every connection is told about.
//
// A capability says the backend HAS a behavior, never that the caller is
// allowed to use it: authorization is re-checked per RPC, and hello flags
// are compatibility hints only (spec §5, frontend capability model).
// Deliberately not per-connection for the same reason — anything that
// varies by who is asking is authorization, and belongs in the scope
// table rather than here.
//
// Rules for a name, which apply from the first entry:
//
//   - It is a stable string that never changes meaning once shipped. A
//     client on an older bundle may still be asking about it. Retiring
//     one means the backend stops advertising it, never that it starts
//     meaning something else.
//   - It names a BEHAVIOR a client can degrade around, not a version, a
//     build flag, or a deployment mode.
//   - Order is fixed so the frame bytes are stable across boots: a
//     diffable log line, and nothing downstream has to sort.
//
// A flag must ship in the SAME release as the behavior it names. Added
// later, it lies about every build in between: those advertise nothing
// while having the behavior, and a client asking the question gets the
// wrong answer forever. That is why the list is not deferred until
// something reads it — the reader can come later, the flag cannot.
var serverCapabilities = []string{
	CapabilityRemoteNotifications,
	CapabilityPasskeys,
}

// serverCapabilitiesWithBrowser is that list plus the one flag whose
// answer is not the same on every deployment. Built once, at init, so an
// accept PICKS a slice instead of allocating one, and appended at the end
// so the frozen prefix's bytes are byte-identical either way.
var serverCapabilitiesWithBrowser = append(slices.Clone(serverCapabilities), CapabilityBrowser)

// advertisedCapabilities answers the set one connection is told about.
//
// The list above is still the rule — a capability names a behavior every
// connection to THIS backend shares, never who is asking. CapabilityBrowser
// is the first entry whose answer is a property of the deployment rather
// than of the build (a serve host with no Chromium installed does not have
// the behavior at all), so it is resolved from the backend once per accept
// and never from the caller.
//
// A nil hook means the same thing false does: no browser tools here.
func advertisedCapabilities(browserAvailable func() bool) []string {
	if browserAvailable != nil && browserAvailable() {
		return serverCapabilitiesWithBrowser
	}
	return serverCapabilities
}

// CapabilityRemoteNotifications says this backend delivers the
// notification channels (`notification:send`, `notification:activated`)
// to non-loopback connections. Before it, both were loopback-only, so an
// attached remote client was told nothing when a turn finished and had no
// way to discover that other than never receiving a frame — indeterminate
// between "no notifications configured" and "backend too old".
//
// It says the frames ARRIVE. It does not say a client should raise
// anything, which is that client's own decision, and it is not
// authorization: emitting stays host-side only.
const CapabilityRemoteNotifications = "notifications.remote"

// CapabilityPasskeys says this backend speaks the passkey ceremonies:
// the sign-in routes on the credential surface, the registration and
// step-up methods on the wire, and ClientFrame.StepUpToken as a proof it
// will actually read. Without it a client cannot tell "this backend
// refuses my step-up" from "this backend is too old to have been asked",
// and the honest fallback — telling the person to go to the machine — is
// only correct in the second case.
//
// It says the ceremonies EXIST. Whether one can be run right now is a
// different, configuration-dependent question the bootstrap manifest
// answers (Bootstrap.PasskeysAvailable), because a backend with no
// canonical domain has nothing to be a relying party for.
const CapabilityPasskeys = "passkeys"

// CapabilityBrowser says this backend can drive a web browser for an
// agent: the browser MCP server is offered to threads, and the browser
// surfaces have something behind them.
//
// It is the one flag that is not settled by the build. A windowed boot
// has an engine because it has a window to host views in; a serve host
// has one only if a Chromium is installed on the machine, and nothing is
// ever downloaded to change that (docs/specs/remote-access.md §7). A
// `--connect` backend has none at all. Without the flag a client cannot
// tell "browser tools turned off in Settings" from "this machine has no
// browser", and only the second one is worth telling somebody how to fix.
//
// It says the tools CAN exist here. Whether they are switched on is the
// owner's setting, and whether a given thread may use one is
// authorization; neither is answered by a hello flag.
const CapabilityBrowser = "browser"

// helloFrame is the first frame written on every upgraded connection.
//
// A separate envelope rather than fields on ServerFrame: ServerFrame is
// built per event and per RPC response, and four more fields would grow
// every one of them to carry something only the connection's first frame
// ever uses. Same reasoning as batchFrame.
//
// Forward tolerance is the contract on the other side: a client that
// does not know this frame type must ignore it, and a client that knows
// it must ignore fields it does not recognise. Both are exercised by the
// future-dialect fixture in the TS suite.
type helloFrame struct {
	Type string `json:"type"`
	// ProtocolVersion states the dialect; see the constant.
	ProtocolVersion int `json:"protocolVersion"`
	// Capabilities is always present, never null: an empty JSON array
	// means "this backend advertises nothing", which a client can read
	// without a nil check. A missing field would be indistinguishable
	// from a backend too old to send the frame at all.
	Capabilities []string `json:"capabilities"`
	// BackendID identifies this backend across clients and reconnects.
	// Empty when the history store has not opened yet — the same rule as
	// the bootstrap manifest, and it means "unknown", never a wildcard.
	BackendID string `json:"backendId,omitempty"`
	// BackendName is what a person calls this machine: its hostname
	// (internal/appidentity.HostDisplayName), which is also the name the
	// pairing payload carries. A client attached to several backends
	// labels its machine picker with it and keeps the pairing-time value
	// as an editable nickname (docs/specs/remote-access.md §10, "Machine
	// name").
	//
	// A DISPLAY string and nothing else: nothing is authorized by it,
	// nothing is keyed on it, and two backends may legitimately answer
	// the same one. BackendID stays the identity. Empty when the
	// hostname is unreadable, which reads as "unknown" — a client falls
	// back to the id rather than to a wildcard.
	BackendName string `json:"backendName,omitempty"`
	// ServerTimeMs is the backend's wall clock at the moment this
	// connection was accepted, in Unix milliseconds. Phones behind
	// captive portals drift, and a silent clock skew is the hardest class
	// of auth failure to debug once signed credentials arrive (spec §9),
	// so the honest measurement is published from the first frame.
	ServerTimeMs int64 `json:"serverTimeMs"`
	// BundleID, BundleVersion and MinShellBuild describe the SPA bundle
	// this backend serves (internal/bundle, bundleroutes.go). They are
	// here rather than on a route because they are read on EVERY
	// connection by the one client that has to compare them against
	// something it already holds, and a shell that had to fetch a
	// document to learn "nothing changed" would pay a round trip per
	// connect forever.
	//
	// Additive, and every consumer but the phone shell ignores them. A
	// backend that cannot build its manifest omits all three, which reads
	// as "this backend does not supply bundles" — the same answer a
	// backend too old to send them gives, and the answer that leaves a
	// shell running what it has.
	//
	// BundleID is the CONTENT id (bundle.Manifest.ID), so the comparison
	// a shell makes is "am I running these exact files", never "is this
	// version newer". BundleVersion is `main.version`, and its only use
	// is ordering: a phone attached to several machines runs the newest
	// attached backend's bundle. MinShellBuild is the lowest Android
	// versionCode this bundle's native seams can run on — the one version
	// gate in the design, and it is here because the capability it names
	// is native code this channel cannot ship.
	BundleID      string `json:"bundleId,omitempty"`
	BundleVersion string `json:"bundleVersion,omitempty"`
	MinShellBuild int    `json:"minShellBuild,omitempty"`
}

// MaxReplayChannels caps the number of channels a single replay request
// can ask the server to scan. An oversized LastSeqByChannel map could
// otherwise force the dispatcher to allocate proportionally large
// response slices.
const MaxReplayChannels = 1024

// MaxSubscribeChannels bounds an opt-in connection event filter. Ordinary
// SPA clients omit subscribe and retain the all-channel behavior; narrow
// service clients use it to avoid receiving unrelated provider traffic.
const MaxSubscribeChannels = 1024

// MaxWatchThreads bounds a connection's watched-entity set. A screen shows
// a handful of panes; 256 is well past any real pane composition and is what
// keeps one frame from making the server build an unbounded map. Each id is
// separately bounded at MaxWatchThreadIDBytes.
const MaxWatchThreads = 256

// MaxWatchThreadIDBytes bounds one entity id in a watch frame. Thread ids
// are short opaque strings; the cap is the same 256 handleSubscribe applies
// to a channel name, for the same reason — the server stores what it is
// given, so the per-item bound is the one that matters.
const MaxWatchThreadIDBytes = 256

// MaxRPCParams caps the number of positional parameters a single RPC
// frame can carry. Protects against pathological inputs that would
// otherwise drive arbitrary reflect.New allocations during decode.
const MaxRPCParams = 64

// ClientFrame is the union of every frame the client may send. The
// receiver dispatches on Type:
//
//   - "rpc": invoke a method. Either MethodID (preferred — matches Wails'
//     reflection hash so generated bindings keep working) or Method (by
//     name, used by the shim's Call.ByName fallback) MUST be set; Params
//     is the positional argument array.
//   - "replay": ask the server to replay any events the client missed
//     while disconnected. LastSeqByChannel maps channel name to the last
//     seq the client successfully applied. Channels absent from the map
//     get no replay.
//   - "subscribe": opt this connection into the live event channels named
//     by Channels. Omitted by ordinary SPA connections, which retain the
//     existing all-visible-channel behavior.
//   - "watch": name the entities (thread ids) this connection is looking
//     at, in Threads. Narrows the EntityFiltered channels only
//     (event_entity.go); every other channel is unaffected. The set is
//     absolute and idempotent, an empty array is legal and means "watching
//     nothing", and a connection that never sends one stays wildcard.
//   - "lease": state this CLIENT's whole-app lifecycle in State, either
//     "active" (the default every connection starts in) or "background".
//     A backgrounded connection has its highlight seeds withheld and its
//     transcript deltas coalesced; everything else flows unchanged. Any
//     other spelling is a bad_params refusal that leaves the lease as it
//     was, and a connection that never sends one behaves exactly as it did
//     before the frame existed. Doctrine and mechanism: lease.go.
//
// Deliberately a SEPARATE frame type from "subscribe" rather than another
// field on it. The two answer different questions — subscribe narrows by
// CHANNEL and watch narrows by ENTITY — and they have different populations:
// the launcher and the harness subscribe to channels and never watch, the
// SPA watches entities and must never subscribe to channels (that is what
// keeps EventBus.ChannelSubscriberCount's "no connected launcher subscriber"
// diagnostic sound). Folding them together would make each one's absence
// ambiguous inside a frame that set the other.
//
// The server echoes ID back on rpc responses so the client can match
// requests to in-flight promises.
type ClientFrame struct {
	Type             string            `json:"type"`
	ID               string            `json:"id,omitempty"`
	Method           string            `json:"method,omitempty"`
	MethodID         uint32            `json:"methodId,omitempty"`
	Params           []json.RawMessage `json:"params,omitempty"`
	LastSeqByChannel map[string]uint64 `json:"lastSeqByChannel,omitempty"`
	Channels         []string          `json:"channels,omitempty"`
	// Threads carries a watch frame's absolute entity set. `omitempty` is
	// correct here and costs nothing: the field is read only for a frame
	// whose Type is already "watch", so an empty array and an absent one
	// mean the same thing on that frame — watching nothing — and no other
	// frame type is affected by its absence.
	Threads []string `json:"threads,omitempty"`
	// State carries a lease frame's whole-client lifecycle: "active" or
	// "background", and nothing else (lease.go). Read only for a frame
	// whose Type is already "lease", so `omitempty` costs nothing and no
	// other frame type is affected by its absence — but an ABSENT state on
	// a lease frame is a refusal, not a default, because "the client meant
	// active" and "the client serialized the field wrong" are the two
	// readings and only one of them is safe to act on.
	State string `json:"state,omitempty"`
	// StepUpToken carries a fresh step-up proof for THIS call and no
	// other: the single-use token a passkey assertion minted, bound to
	// the session this connection named (§4 "Step-up"). Empty on every
	// ordinary frame, and additive — a backend that does not read it
	// behaves exactly as before, which is what makes the swap window safe.
	//
	// On the frame rather than in Params, because it is not an argument:
	// no method's signature mentions it, the argument-dependent rechecks
	// inside a method need it just as much as the pre-call gate does, and
	// a parameter would have to be threaded through every generated
	// binding that could ever need one.
	//
	// Presenting it SPENDS it, whatever the call turns out to need. A
	// client attaches one to the call it is retrying and nowhere else.
	StepUpToken string `json:"stepUpToken,omitempty"`
}

// ServerFrame is the union of every frame the server may send. The
// receiver dispatches on Type:
//
//   - "rpc": response to a prior client rpc. ID matches the client's
//     request. Exactly one of Result or Error is populated.
//   - "event": pushed event. Channel + Seq + Data carry the payload. Gap
//     is true when the client's replay request fell outside the in-memory
//     ring — the client should re-fetch via list endpoints rather than
//     rely on the (truncated) history.
//   - "batch": coalesced pushed events in the batchFrame envelope below.
//   - "replay": completion marker sent after a replay request. Replay and
//     live pushes can interleave, so strict-order consumers buffer until it.
//   - "ping": server keepalive heartbeat (conn.go keepalive loop). Carries
//     no other fields. Clients treat its arrival as a liveness signal for
//     stale-connection detection and otherwise ignore it; consumers that
//     switch on known types skip it for free.
type ServerFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *FrameError     `json:"error,omitempty"`
	Channel string          `json:"channel,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Gap     bool            `json:"gap,omitempty"`
}

// FrameError is the server's error envelope on an rpc response. Code is
// a stable machine-readable token; Message is human-readable. We keep
// both so the frontend can switch on Code without parsing prose.
type FrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Reason is set only alongside ErrCodeAuthFailed, and carries the
	// stable spelling of one member of internal/identity's closed refusal
	// set. It is a plain string here on purpose: transport does not import
	// identity (that direction would pull the store in behind it), so this
	// field is carriage, and identity owns the vocabulary.
	//
	// The frontend maps it to an actionable hint in
	// frontend/src/lib/transport/authReason.ts; a code that module does not
	// know degrades to the generic message rather than showing nothing.
	//
	// Omitted on every other error, so a client that reads it as
	// "was this an auth refusal, and why" gets an unambiguous answer.
	Reason string `json:"reason,omitempty"`
	// Scope is set only alongside ErrCodeScopeRequired, and names the
	// capability the caller's session was not granted — one member of
	// scopes.go's set, `host` included (which no session can hold, and
	// which the message says so about).
	//
	// A field rather than prose to parse, for the reason Code is one: a
	// method error's TEXT does not survive the wire for a non-loopback
	// caller, and this is exactly what such a caller must branch on to
	// explain a disabled surface rather than showing a dead control
	// (docs/specs/remote-access.md §5 "Frontend capability model").
	Scope string `json:"scope,omitempty"`
}

// Error codes returned on rpc responses. Stable strings so the frontend
// can branch on them without string-matching prose.
//
//   - method_not_found: dispatcher saw no registered method for the id/name.
//   - bad_params:       JSON decode failed or arity/type was wrong on input.
//   - method_error:     the registered method returned a non-nil error.
//     Wire message contains a generic prose; the full
//     error is logged server-side under a correlation id.
//   - temporarily_unavailable: the method could not complete before its
//     bounded deadline. Retrying is safe and may succeed.
//   - already_handled: the thing this call would have decided was already
//     decided, by another client or by this one. Not a failure — the
//     caller's intent is satisfied, just not by this call. Retrying can
//     never succeed.
//   - internal:         dispatcher panicked or hit an internal failure.
//     Wire message is generic; full panic + stack is
//     logged server-side under a correlation id.
//   - shutting_down:    server is mid-shutdown; this RPC was dropped.
//   - auth_failed:      the caller's session credential did not admit this
//     call. The FrameError carries a Reason naming which
//     check refused it; see the field.
//
// Two more live in authorize.go beside the gate that produces them:
// scope_required and step_up_required (docs/specs/remote-access.md §5).
const (
	ErrCodeMethodNotFound         = "method_not_found"
	ErrCodeBadParams              = "bad_params"
	ErrCodeMethodError            = "method_error"
	ErrCodeTemporarilyUnavailable = "temporarily_unavailable"
	ErrCodeAlreadyHandled         = "already_handled"
	ErrCodeInternal               = "internal"
	ErrCodeShuttingDown           = "shutting_down"
	ErrCodeAuthFailed             = "auth_failed"
)

// AuthFailure builds the refusal envelope for a caller whose session
// credential did not admit a call.
//
// One constructor rather than a struct literal per site, because the two
// fields are only meaningful together: a Reason on any other code is
// unreadable by the client's hint module, and an auth_failed with no
// reason degrades every refusal to the same generic prose. Building them
// apart is exactly the mistake that would ship.
//
// The message stays generic. It is the code and the reason that carry
// meaning across the wire, and prose is redacted for non-loopback callers
// anyway (§ Credentials and refusal shapes in AGENTS.md).
//
// A refusal the backend cannot attribute to a session at all is NOT this:
// that is answered before the request reaches a method, with the
// unfingerprintable 404 the credential channel has always used.
func AuthFailure(reasonCode string) *FrameError {
	return &FrameError{
		Code:    ErrCodeAuthFailed,
		Message: "not authorized",
		Reason:  reasonCode,
	}
}

// ErrTemporarilyUnavailable marks a method failure as transient at the RPC
// boundary. Methods wrap this sentinel together with their concrete cause;
// the dispatcher preserves the usual loopback/LAN message-redaction policy
// while exposing the stable code clients need to offer a truthful retry.
var ErrTemporarilyUnavailable = errors.New("temporarily unavailable")

// ErrAlreadyHandled marks a decision another caller already made. It exists
// because one backend now serves several screens, and two of them can hold
// the same open question — an approval prompt, a queued message — and answer
// it within the same second. The backend is the single writer and one answer
// wins; the loser's call did not fail, it arrived second.
//
// The distinction is worth a code because the two outcomes want opposite
// treatment. A method_error is a problem to report and possibly retry; an
// already_handled is the state the caller wanted, reached without them, and
// the honest UI response is to drop the prompt quietly rather than to raise
// an error about a question that is no longer open. Retrying can never
// succeed, so a client must not offer it.
var ErrAlreadyHandled = errors.New("already handled")

// batchEventEntry is one event inside a batch frame. It carries the
// subset of Event fields the client needs to dispatch: channel, seq,
// data, and the gap flag. Since batch frames are spliced from
// pre-encoded event envelopes (spliceBatchFrame), each entry on the
// wire additionally carries an inert `"type":"event"` field that every
// consumer ignores; this struct remains the consumer-side parse shape
// (tests and the wsllauncher notification client decode through it).
type batchEventEntry struct {
	Channel string          `json:"channel"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	Gap     bool            `json:"gap,omitempty"`
}

// batchFrame is the server-side envelope for coalesced event delivery.
// Produced by the per-connection coalescing writer whenever a flush
// window holds more than one event; single-event windows ship as plain
// "event" frames. Wire shape: {"type":"batch","events":[...]},
// assembled by spliceBatchFrame from per-event WireBytes rather than
// marshalled through this struct.
type batchFrame struct {
	Type   string            `json:"type"`
	Events []batchEventEntry `json:"events"`
}

// batchFramePrefix is the fixed opening of a spliced batch frame; the
// splice appends comma-joined event envelopes and closes with "]}".
const batchFramePrefix = `{"type":"` + frameTypeBatch + `","events":[`
