package rollout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// DefaultMaxLineBytes caps one rollout line. Codex writes whole tool outputs
// inline, so lines of a few MB are ordinary; anything past this is treated as
// corruption rather than something to buffer.
const DefaultMaxLineBytes = 32 << 20

// scanBufferSize is the reader window for a full conversion pass. Lines above
// it are assembled across ReadSlice calls; it only decides how often that
// happens.
const scanBufferSize = 256 << 10

// headScanBufferSize is the reader window for a head read that wants ONE small
// record — ReadSessionMeta's `session_meta` line, a few hundred bytes even
// with git provenance on it. Fork-ancestor exclusion calls that once per
// candidate session, and a real Codex home holds hundreds, so the conversion
// pass's window would allocate tens of megabytes to read a few kilobytes. A
// longer line still reads correctly, just across more ReadSlice calls.
const headScanBufferSize = 8 << 10

// scanLine is one newline-terminated rollout line.
//
// Start/Next are absolute byte offsets: Start is the line's first byte and
// Next is the first byte AFTER its newline — that is, the offset a later
// read may resume from. Data aliases the scanner's buffer and is only valid
// until the next call.
type scanLine struct {
	Data      []byte
	Start     int64
	Next      int64
	Oversized bool
}

// scanner walks a rollout file line by line with exact byte offsets.
//
// It deliberately does not use bufio.Scanner: an over-long token there is a
// terminal error that abandons the rest of the file, whereas one corrupt
// giant line must only cost us that line.
//
// A trailing line with no newline is NEVER returned. Rollouts are appended to
// while we read them, so an unterminated tail is a half-written record; not
// consuming it keeps EndOffset on a record boundary and lets a later refresh
// read the line whole.
type scanner struct {
	r      *bufio.Reader
	max    int
	offset int64
	buf    []byte
	done   bool
}

// newScanner builds a scanner over r. bufferSize is the read window only —
// it bounds allocation, never what the scanner can return; maxLineBytes is
// the cap that decides a line is corrupt.
func newScanner(r io.Reader, offset int64, maxLineBytes, bufferSize int) *scanner {
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxLineBytes
	}
	if bufferSize <= 0 {
		bufferSize = scanBufferSize
	}
	return &scanner{
		r:      bufio.NewReaderSize(r, bufferSize),
		max:    maxLineBytes,
		offset: offset,
	}
}

// next returns the next complete line, or io.EOF when none remains.
func (s *scanner) next() (scanLine, error) {
	if s.done {
		return scanLine{}, io.EOF
	}
	start := s.offset
	s.buf = s.buf[:0]
	oversized := false
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.offset += int64(len(chunk))
		if !oversized && len(s.buf)+len(chunk) > s.max {
			// Past the cap: stop accumulating but keep draining to the
			// newline so the following lines stay readable.
			oversized = true
			s.buf = s.buf[:0]
		}
		if !oversized {
			s.buf = append(s.buf, chunk...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			// Unterminated tail. Rewind the offset so EndOffset stays on
			// the last complete record.
			s.done = true
			s.offset = start
			return scanLine{}, io.EOF
		case err != nil:
			s.done = true
			return scanLine{}, err
		}
		return scanLine{
			Data:      bytes.TrimRight(s.buf, "\r\n"),
			Start:     start,
			Next:      s.offset,
			Oversized: oversized,
		}, nil
	}
}

// envelope is the rollout line frame: {"timestamp", "type", "payload"}.
//
// `type` is an open enum. Codex has renamed and added variants between
// releases (a 0.146 file carries `inter_agent_communication_metadata`, which
// the 0.142 source calls `inter_agent_communication`), so every consumer here
// treats an unrecognised value as skip-and-count, never as an error.
type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// payloadKind is the inner tag on `event_msg` / `response_item` payloads.
type payloadKind struct {
	Type string `json:"type"`
}

// decodeEnvelope decodes one line. ok=false means the line is unusable
// (invalid JSON, embedded NUL, or no type) and the caller counts it as
// corrupt.
func decodeEnvelope(data []byte) (envelope, bool) {
	if len(data) == 0 || bytes.IndexByte(data, 0) >= 0 {
		return envelope{}, false
	}
	var env envelope
	if json.Unmarshal(data, &env) != nil {
		return envelope{}, false
	}
	if env.Type == "" {
		return envelope{}, false
	}
	return env, true
}

// payloadType returns the inner tag, or "" when the payload has none.
func payloadType(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var kind payloadKind
	if json.Unmarshal(payload, &kind) != nil {
		return ""
	}
	return kind.Type
}

// qualifiedType is the census key an unrecognised line is counted under:
// "event_msg/foo" and "response_item/bar" for the two tagged containers, the
// bare envelope type otherwise.
func qualifiedType(env envelope) string {
	switch env.Type {
	case typeEventMsg, typeResponseItem:
		if inner := payloadType(env.Payload); inner != "" {
			return env.Type + "/" + inner
		}
		return env.Type + "/?"
	default:
		return env.Type
	}
}

// parseTimestamp reads a rollout timestamp. Codex writes RFC3339 with
// milliseconds; a line whose timestamp is missing or malformed yields the
// zero time and the caller falls back to the last good one, because an
// imported row with a 1970 timestamp would sort ahead of everything.
func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return ts
}
