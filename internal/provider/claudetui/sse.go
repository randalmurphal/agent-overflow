package claudetui

import (
	"bytes"
	"encoding/json"
)

// sseScanner extracts the JSON payloads of Server-Sent-Events `data:` lines
// from a byte stream that arrives in arbitrary chunks. Anthropic's
// /v1/messages stream frames one JSON event per `data:` line; the `event:`
// type line is redundant (the JSON carries its own `type`), so we only collect
// data payloads.
//
// The scanner is fed by the gateway's response copy loop and drives one
// reconstruction callback per complete event. It buffers a partial trailing
// line across chunk boundaries so an event split mid-line is never dropped.
type sseScanner struct {
	buf bytes.Buffer
	on  func(event json.RawMessage)
}

// newline is the SSE line separator, hoisted so the scan loop doesn't
// re-materialize the one-byte slice on every iteration.
var newline = []byte{'\n'}

func newSSEScanner(on func(json.RawMessage)) *sseScanner {
	return &sseScanner{on: on}
}

// write feeds the next chunk of response bytes, emitting every complete
// `data:` event it now contains.
func (s *sseScanner) write(p []byte) {
	s.buf.Write(p)
	for {
		line, rest, found := bytes.Cut(s.buf.Bytes(), newline)
		if !found {
			return // partial line — wait for more bytes
		}
		// Re-seat the buffer on the unconsumed remainder before dispatching,
		// so a re-entrant callback never sees a stale buffer.
		consumed := s.buf.Len() - len(rest)
		s.buf.Next(consumed)
		s.emitLine(line)
	}
}

func (s *sseScanner) emitLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !json.Valid(payload) {
		return // a non-JSON data line (keep-alive comment etc.) — skip
	}
	// Copy: the underlying buffer storage is reused as more bytes arrive.
	event := make(json.RawMessage, len(payload))
	copy(event, payload)
	s.on(event)
}
