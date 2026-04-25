package transport

import (
	"encoding/json"
	"testing"
)

// Frame round-trip: every wire shape we accept must survive JSON
// marshal+unmarshal without losing fields. A dropped field would
// surface as a silent protocol bug, so this test is the cheap guard.

func TestClientFrame_RPCRoundTrip(t *testing.T) {
	in := ClientFrame{
		Type:     frameTypeRPC,
		ID:       "abc",
		MethodID: 1352159878,
		Params:   []json.RawMessage{json.RawMessage(`"proj-1"`)},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ClientFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.ID != in.ID || out.MethodID != in.MethodID {
		t.Fatalf("rpc fields lost: in=%+v out=%+v", in, out)
	}
	if len(out.Params) != 1 || string(out.Params[0]) != `"proj-1"` {
		t.Fatalf("params lost: %v", out.Params)
	}
}

func TestClientFrame_ReplayRoundTrip(t *testing.T) {
	in := ClientFrame{
		Type:             frameTypeReplay,
		LastSeqByChannel: map[string]uint64{"thread:updated": 42, "provider:item_event": 1000},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ClientFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type {
		t.Fatalf("type lost")
	}
	if out.LastSeqByChannel["thread:updated"] != 42 || out.LastSeqByChannel["provider:item_event"] != 1000 {
		t.Fatalf("lastSeqByChannel lost: %v", out.LastSeqByChannel)
	}
}

func TestServerFrame_EventRoundTrip(t *testing.T) {
	in := ServerFrame{
		Type:    frameTypeEvent,
		Channel: "thread:updated",
		Seq:     7,
		Data:    json.RawMessage(`{"id":"thread-1","title":"hi"}`),
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ServerFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.Channel != in.Channel || out.Seq != in.Seq {
		t.Fatalf("event fields lost: in=%+v out=%+v", in, out)
	}
	if string(out.Data) != string(in.Data) {
		t.Fatalf("data lost: %s", string(out.Data))
	}
}

func TestServerFrame_GapMarker(t *testing.T) {
	in := ServerFrame{
		Type:    frameTypeEvent,
		Channel: "provider:item_event",
		Seq:     500,
		Gap:     true,
		Data:    json.RawMessage(`null`),
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(buf), `"gap":true`) {
		t.Fatalf("gap flag dropped: %s", string(buf))
	}
}

func TestServerFrame_RPCError(t *testing.T) {
	in := ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-7",
		Error: &FrameError{Code: ErrCodeMethodNotFound, Message: "no method"},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ServerFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil {
		t.Fatalf("error envelope dropped")
	}
	if out.Error.Code != ErrCodeMethodNotFound {
		t.Fatalf("error code lost: %s", out.Error.Code)
	}
	// On a successful error frame, Result should be empty (omitempty).
	if !contains(string(buf), `"error"`) || contains(string(buf), `"result"`) {
		t.Fatalf("error frame leaked result field: %s", string(buf))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
