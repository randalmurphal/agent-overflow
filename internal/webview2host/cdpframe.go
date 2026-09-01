package webview2host

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The CDP tunnel wire.
//
// One WebSocket carries every CDP connection the backend wants. Control
// rides TEXT frames as JSON; payload rides BINARY frames prefixed with a
// 4-byte big-endian stream id. Splitting by frame type rather than by an
// envelope keeps the payload path allocation-free apart from the prefix,
// which matters: CDP screencasts and DOM snapshots are the bulk traffic.
//
// The backend opens; the launcher answers opened or error and then pipes.
// Either side may close. There is no reconnect-and-resume: a dropped
// tunnel drops every stream, and the backend re-opens what it still
// wants.

// Tunnel control ops.
const (
	// TunnelOpen is backend -> launcher: dial the registered CDP port and
	// bind the result to StreamID.
	TunnelOpen = "open"
	// TunnelOpened is launcher -> backend: the dial succeeded.
	TunnelOpened = "opened"
	// TunnelError is launcher -> backend: the dial or the stream failed.
	// Detail says why; the stream id is dead either way.
	TunnelError = "error"
	// TunnelClose is either direction: tear this stream down.
	TunnelClose = "close"
)

// TunnelControl is one text frame.
type TunnelControl struct {
	Op       string `json:"op"`
	StreamID uint32 `json:"streamId"`
	Detail   string `json:"detail,omitempty"`
}

// MaxTunnelStreams bounds concurrent streams on one tunnel. chromedp
// holds one browser-level connection plus one per attached target, so a
// pane with a handful of tabs sits in single digits; 64 is a ceiling that
// only a leak or a hostile backend reaches, and reaching it must not let
// the launcher open unbounded sockets.
const MaxTunnelStreams = 64

// MaxTunnelFrameBytes is the per-frame read limit on both directions.
// CDP messages are JSON and screenshots arrive base64-encoded inside
// them, so this is generous rather than tight; it exists so one frame
// cannot make the launcher allocate without bound.
const MaxTunnelFrameBytes = 1 << 20

// TunnelChunkBytes is how much the launcher reads from a CDP socket
// before writing a data frame. Comfortably under MaxTunnelFrameBytes so
// the far side's own limit is never the thing that trips.
const TunnelChunkBytes = 32 << 10

// ErrShortDataFrame reports a binary frame with no room for the stream id.
var ErrShortDataFrame = errors.New("cdp tunnel data frame is shorter than its stream id prefix")

// EncodeTunnelData writes id's prefix into dst[:4] and returns the frame
// slice. dst must have room for the prefix plus payload; callers reuse
// one buffer per stream so the hot path allocates nothing.
func EncodeTunnelData(dst []byte, id uint32, payload []byte) []byte {
	if cap(dst) < 4+len(payload) {
		dst = make([]byte, 4+len(payload))
	}
	dst = dst[:4+len(payload)]
	binary.BigEndian.PutUint32(dst[:4], id)
	copy(dst[4:], payload)
	return dst
}

// DecodeTunnelData splits a binary frame into its stream id and payload.
// The payload aliases frame; callers that keep it must copy.
func DecodeTunnelData(frame []byte) (uint32, []byte, error) {
	if len(frame) < 4 {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrShortDataFrame, len(frame))
	}
	return binary.BigEndian.Uint32(frame[:4]), frame[4:], nil
}
