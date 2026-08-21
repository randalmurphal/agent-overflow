package claude

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	fn()
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
	return buf.String()
}

func TestSessionHasCapability(t *testing.T) {
	s := &Session{threadID: "t1"}
	if s.HasCapability("interrupt_receipt_v1") {
		t.Fatal("pre-init HasCapability must be false")
	}
	s.noteCapabilities([]string{"interrupt_receipt_v1", "msg_lifecycle_v1"})
	if !s.HasCapability("interrupt_receipt_v1") || !s.HasCapability("msg_lifecycle_v1") {
		t.Fatalf("Capabilities = %+v", s.Capabilities())
	}
	if s.HasCapability("queued_notifications") {
		t.Fatal("an unadvertised token must answer false")
	}
	if s.HasCapability("") || s.HasCapability("  ") {
		t.Fatal("blank names must answer false")
	}
	var nilSession *Session
	if nilSession.HasCapability("interrupt_receipt_v1") {
		t.Fatal("nil session must answer false")
	}
	if got := nilSession.Capabilities(); got != nil {
		t.Fatalf("nil session Capabilities = %+v", got)
	}
}

// `system/init` is re-emitted before EVERY turn (spike-observed), so the
// capability ingest runs once per turn for the life of the process. It
// must be idempotent and it must not re-fire the once-per-session debug
// log — otherwise a long thread prints the same line hundreds of times.
func TestSessionNoteCapabilities_SecondInitDoesNotDoubleFire(t *testing.T) {
	s := &Session{threadID: "t1"}
	caps := []string{"interrupt_receipt_v1", "msg_lifecycle_v1"}

	first := captureLog(t, func() { s.noteCapabilities(caps) })
	if strings.Count(first, "advertises capabilities") != 1 {
		t.Fatalf("first init log = %q, want exactly one capability line", first)
	}

	second := captureLog(t, func() {
		// Three more inits, the way three more turns would deliver them.
		s.noteCapabilities(caps)
		s.noteCapabilities(caps)
		s.noteCapabilities([]string{"msg_lifecycle_v1", "interrupt_receipt_v1"})
	})
	if strings.Contains(second, "advertises capabilities") {
		t.Fatalf("re-init logged again: %q", second)
	}
	if got := s.Capabilities(); len(got) != 2 {
		t.Fatalf("Capabilities = %+v, want the same two tokens after repeats", got)
	}
}

// An EMPTY set is "the envelope said nothing" (older CLI, or a build with
// no tokens) — never "this session lost its capabilities". Same absence
// rule the slash-command list follows.
func TestSessionNoteCapabilities_EmptyDoesNotClear(t *testing.T) {
	s := &Session{threadID: "t1"}
	s.noteCapabilities([]string{"interrupt_receipt_v1"})
	s.noteCapabilities(nil)
	s.noteCapabilities([]string{})
	s.noteCapabilities([]string{"", "   "})
	if !s.HasCapability("interrupt_receipt_v1") {
		t.Fatalf("an empty init cleared the set: %+v", s.Capabilities())
	}
}

// A later init that advertises a DIFFERENT set replaces it wholesale —
// the envelope restates the whole answer, exactly like slash_commands.
func TestSessionNoteCapabilities_LaterInitReplacesTheSet(t *testing.T) {
	s := &Session{threadID: "t1"}
	s.noteCapabilities([]string{"interrupt_receipt_v1"})
	s.noteCapabilities([]string{"msg_lifecycle_v1"})
	if s.HasCapability("interrupt_receipt_v1") {
		t.Fatal("a token dropped by a later init must not survive")
	}
	if !s.HasCapability("msg_lifecycle_v1") {
		t.Fatal("the new set must apply")
	}
}

// The parse half of the same idempotency claim: two identical init
// envelopes produce identical SessionInfo, so nothing downstream can
// accumulate.
func TestParseSystem_RepeatedInitIsIdempotent(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-fable-5","capabilities":["interrupt_receipt_v1","msg_lifecycle_v1"],"slash_commands":["compact"]}`)

	decode := func() provider.SessionInfo {
		events, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("events = %+v, want exactly one init event per envelope", events)
		}
		var info provider.SessionInfo
		if err := json.Unmarshal(events[0].Meta, &info); err != nil {
			t.Fatalf("meta unmarshal: %v", err)
		}
		return info
	}

	first := decode()
	second := decode()
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("repeated init diverged:\n first=%s\nsecond=%s", firstJSON, secondJSON)
	}
	if len(second.Capabilities) != 2 {
		t.Fatalf("Capabilities = %+v, want two tokens (no accumulation)", second.Capabilities)
	}
}
