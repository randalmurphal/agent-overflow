package transport

import (
	"encoding/json"
	"errors"
)

// FrameType discriminates the wire frames. Encoded as the "type" field
// in every frame so the decoder can route without sniffing other fields.
const (
	frameTypeRPC       = "rpc"
	frameTypeEvent     = "event"
	frameTypeReplay    = "replay"
	frameTypeSubscribe = "subscribe"
	frameTypeBatch     = "batch"
	frameTypePing      = "ping"
)

// MaxReplayChannels caps the number of channels a single replay request
// can ask the server to scan. A maliciously oversized LastSeqByChannel
// map could otherwise force the dispatcher to allocate proportionally
// large response slices.
const MaxReplayChannels = 1024

// MaxSubscribeChannels bounds an opt-in connection event filter. Ordinary
// SPA clients omit subscribe and retain the all-channel behavior; narrow
// service clients use it to avoid receiving unrelated provider traffic.
const MaxSubscribeChannels = 1024

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
//   - internal:         dispatcher panicked or hit an internal failure.
//     Wire message is generic; full panic + stack is
//     logged server-side under a correlation id.
//   - shutting_down:    server is mid-shutdown; this RPC was dropped.
const (
	ErrCodeMethodNotFound         = "method_not_found"
	ErrCodeBadParams              = "bad_params"
	ErrCodeMethodError            = "method_error"
	ErrCodeTemporarilyUnavailable = "temporarily_unavailable"
	ErrCodeInternal               = "internal"
	ErrCodeShuttingDown           = "shutting_down"
)

// ErrTemporarilyUnavailable marks a method failure as transient at the RPC
// boundary. Methods wrap this sentinel together with their concrete cause;
// the dispatcher preserves the usual loopback/LAN message-redaction policy
// while exposing the stable code clients need to offer a truthful retry.
var ErrTemporarilyUnavailable = errors.New("temporarily unavailable")

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
