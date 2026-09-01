package webview2host

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestTunnelDataRoundTrip(t *testing.T) {
	payload := []byte(`{"id":7,"method":"Page.navigate"}`)
	frame := EncodeTunnelData(nil, 0xDEADBEEF, payload)

	id, got, err := DecodeTunnelData(frame)
	if err != nil {
		t.Fatalf("DecodeTunnelData: %v", err)
	}
	if id != 0xDEADBEEF {
		t.Fatalf("stream id = %#x, want 0xDEADBEEF", id)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

// The prefix is big-endian by contract; the backend decodes it with the
// same rule, so an endianness flip here would be an invisible break.
func TestTunnelDataPrefixIsBigEndian(t *testing.T) {
	frame := EncodeTunnelData(nil, 1, nil)
	if !bytes.Equal(frame, []byte{0, 0, 0, 1}) {
		t.Fatalf("frame = %v, want [0 0 0 1]", frame)
	}
}

func TestEncodeTunnelDataReusesTheCallerBuffer(t *testing.T) {
	buf := make([]byte, 0, 4+16)
	frame := EncodeTunnelData(buf, 2, []byte("hello"))
	if &frame[0] != &buf[:1][0] {
		t.Fatal("EncodeTunnelData allocated instead of reusing a buffer with room")
	}
	// And grows when it must, rather than truncating.
	frame = EncodeTunnelData(buf, 2, make([]byte, 64))
	if len(frame) != 4+64 {
		t.Fatalf("frame length = %d, want %d", len(frame), 4+64)
	}
}

func TestDecodeTunnelDataRejectsAShortFrame(t *testing.T) {
	for _, frame := range [][]byte{nil, {}, {1}, {1, 2, 3}} {
		if _, _, err := DecodeTunnelData(frame); !errors.Is(err, ErrShortDataFrame) {
			t.Fatalf("DecodeTunnelData(%v) error = %v, want ErrShortDataFrame", frame, err)
		}
	}
	// Exactly four bytes is a legal empty payload, not an error.
	if _, payload, err := DecodeTunnelData([]byte{0, 0, 0, 9}); err != nil || len(payload) != 0 {
		t.Fatalf("DecodeTunnelData(prefix only) = %v, %v", payload, err)
	}
}

func TestTunnelControlJSONShape(t *testing.T) {
	encoded, err := json.Marshal(TunnelControl{Op: TunnelOpened, StreamID: 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"op":"opened","streamId":3}` {
		t.Fatalf("encoded = %s, want the documented shape", encoded)
	}
	var control TunnelControl
	if err := json.Unmarshal([]byte(`{"op":"error","streamId":4,"detail":"refused"}`), &control); err != nil {
		t.Fatal(err)
	}
	if control != (TunnelControl{Op: TunnelError, StreamID: 4, Detail: "refused"}) {
		t.Fatalf("decoded = %#v", control)
	}
}
