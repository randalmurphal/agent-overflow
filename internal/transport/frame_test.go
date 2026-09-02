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

// TestServerFrame_ScopeRefusalRoundTrip — the missing capability rides a
// FIELD, so it has to survive marshal/unmarshal under the exact JSON name
// the client reads (frontend/src/lib/transport/frames.ts). Prose does not
// survive the wire for a non-loopback caller, so if the field is lost the
// only thing left to explain a disabled surface is a generic message.
func TestServerFrame_ScopeRefusalRoundTrip(t *testing.T) {
	in := ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-9",
		Error: scopeRefusal(ScopeGitOperate, "GitPruneBranches"),
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(buf), `"scope":"git:operate"`) {
		t.Fatalf("the scope name is not on the wire under the name the client reads: %s", buf)
	}
	var out ServerFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != ErrCodeScopeRequired {
		t.Fatalf("refusal code lost: %+v", out.Error)
	}
	if out.Error.Scope != string(ScopeGitOperate) {
		t.Fatalf("scope field lost: %q", out.Error.Scope)
	}

	// Every OTHER error omits it, so a client reading "was this an
	// authorization refusal, and for what" gets an unambiguous answer.
	plain, err := json.Marshal(ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-10",
		Error: &FrameError{Code: ErrCodeMethodError, Message: "boom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(plain), `"scope"`) {
		t.Fatalf("an ordinary method error carried a scope field: %s", plain)
	}
	stepUp, err := json.Marshal(ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-11",
		Error: &FrameError{Code: ErrCodeStepUpRequired, Message: "needs a proof"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(stepUp), `"scope"`) {
		t.Fatalf("a step-up refusal named a scope; no grant satisfies it: %s", stepUp)
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

func TestClientFrame_LeaseRoundTrip(t *testing.T) {
	in := ClientFrame{Type: frameTypeLease, State: leaseStateBackground}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// The wire shape the phone shell writes, spelled out: what a client
	// reads is this string, not this struct.
	if string(buf) != `{"type":"lease","state":"background"}` {
		t.Fatalf("lease frame on the wire = %s", buf)
	}
	var out ClientFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.State != in.State {
		t.Fatalf("lease fields lost: in=%+v out=%+v", in, out)
	}
}

// TestClientFrameVocabularyIsFrozen pins the set of frame types a client may
// send. A frame type is a wire contract two codebases hold: adding one means
// teaching frontend/src/lib/transport/frames.ts the same word, documenting it
// on ClientFrame, and routing it in readLoop. Freezing the list makes that a
// deliberate edit rather than something the compiler waves through.
func TestClientFrameVocabularyIsFrozen(t *testing.T) {
	frozen := []string{
		frameTypeRPC,
		frameTypeReplay,
		frameTypeSubscribe,
		frameTypeWatch,
		frameTypeLease,
	}
	want := map[string]bool{
		"rpc": true, "replay": true, "subscribe": true, "watch": true, "lease": true,
	}
	if len(frozen) != len(want) {
		t.Fatalf("client frame vocabulary changed: %v", frozen)
	}
	for _, name := range frozen {
		if !want[name] {
			t.Fatalf("unfrozen client frame type %q", name)
		}
	}
}
