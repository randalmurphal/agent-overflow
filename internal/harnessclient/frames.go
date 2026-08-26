package harnessclient

import "encoding/json"

// The wire frames, mirroring internal/transport/frame.go. Only the
// fields a client reads or writes are declared; the server tolerates
// omitted optional fields, and a client that restated the full struct
// would acquire a maintenance burden it cannot discharge.

const (
	frameTypeRPC       = "rpc"
	frameTypeEvent     = "event"
	frameTypeReplay    = "replay"
	frameTypeSubscribe = "subscribe"
	frameTypeBatch     = "batch"
	frameTypePing      = "ping"
)

// clientFrame is everything this client sends. Method is spelled by NAME
// rather than by the Wails FNV method id: a CLI has no generated
// bindings, and name dispatch is the documented path for the harness
// receiver (internal/transport/AGENTS.md).
type clientFrame struct {
	Type             string            `json:"type"`
	ID               string            `json:"id,omitempty"`
	Method           string            `json:"method,omitempty"`
	Params           []json.RawMessage `json:"params,omitempty"`
	LastSeqByChannel map[string]uint64 `json:"lastSeqByChannel,omitempty"`
	Channels         []string          `json:"channels,omitempty"`
}

// serverFrame is the union the read loop dispatches on. A batch frame
// splices pre-encoded event envelopes, so its entries decode through the
// same eventEntry shape a single event frame does.
type serverFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *frameError     `json:"error,omitempty"`
	Channel string          `json:"channel,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Gap     bool            `json:"gap,omitempty"`
	Events  []eventEntry    `json:"events,omitempty"`
}

type eventEntry struct {
	Channel string          `json:"channel"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	Gap     bool            `json:"gap,omitempty"`
}

// frameError is the server's rpc error envelope: a stable machine code
// plus human prose. Both are surfaced — a CLI that printed only the
// prose would hide the one field a script can branch on.
type frameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RPCError is a method refusal or failure reported by the server.
type RPCError struct {
	Method  string
	Code    string
	Message string
}

func (e *RPCError) Error() string {
	return e.Method + ": " + e.Code + ": " + e.Message
}
