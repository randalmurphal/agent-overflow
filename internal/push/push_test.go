package push

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/notify"
)

// A presentation carries the kind's fixed phrase and the machine's name,
// and NOTHING about the thread beyond the id the tap routes on. That is
// §9's redaction rule for a payload that transits Google, and it is the
// property this package exists to make structural.
func TestAPresentationCarriesAFixedPhraseAndTheBackendName(t *testing.T) {
	send := notify.Send{
		ID:     "thread:t-1",
		Kind:   notify.KindTurnComplete,
		Title:  "Rewrite the parser",
		Body:   "Completed",
		Target: notify.Target{Kind: "thread", ThreadID: "t-1", BackendID: "backend-9"},
	}

	message, err := MessageFor(send, "device-token", "Studio")
	if err != nil {
		t.Fatalf("MessageFor: %v", err)
	}

	if message.Token != "device-token" {
		t.Errorf("Token = %q, want the token it was given", message.Token)
	}
	if message.Tag != send.ID {
		t.Errorf("Tag = %q, want the send id %q: the tag is what replace and retract find", message.Tag, send.ID)
	}
	want := map[string]string{
		KeyID:     "thread:t-1",
		KeyKind:   "turn-complete",
		KeyTitle:  "Turn complete",
		KeyBody:   "Studio",
		KeyTarget: `{"kind":"thread","threadId":"t-1","backendId":"backend-9"}`,
	}
	for key, value := range want {
		if message.Data[key] != value {
			t.Errorf("data[%q] = %q, want %q", key, message.Data[key], value)
		}
	}
	if len(message.Data) != len(want) {
		t.Errorf("data has %d keys (%v), want exactly %d", len(message.Data), message.Data, len(want))
	}
	for key, value := range message.Data {
		if value == send.Title {
			t.Errorf("data[%q] carries the thread title; a push payload transits Google (§9)", key)
		}
	}
}

// The target survives as the SAME document the SPA's activation parser
// reads, which is the whole reason it rides as one key rather than as
// flattened siblings — `Target.Kind` and the notification kind are both
// spelled "kind" and would collide.
func TestTheTargetRidesAsItsOwnDocument(t *testing.T) {
	message, err := MessageFor(notify.Send{
		ID:     "approval:t-2:r-7",
		Kind:   notify.KindApprovalNeeded,
		Title:  "Some thread",
		Body:   "Pending approval: Bash",
		Target: notify.Target{Kind: "thread", ThreadID: "t-2"},
	}, "device-token", "Studio")
	if err != nil {
		t.Fatalf("MessageFor: %v", err)
	}
	if message.Data[KeyKind] != string(notify.KindApprovalNeeded) {
		t.Fatalf("data[kind] = %q, want the notification kind", message.Data[KeyKind])
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(message.Data[KeyTarget]), &decoded); err != nil {
		t.Fatalf("the target key must be a JSON document: %v", err)
	}
	if decoded["kind"] != "thread" || decoded["threadId"] != "t-2" {
		t.Fatalf("target decoded to %v, want the route it was given", decoded)
	}
	if message.Data[KeyTitle] != "Approval needed" {
		t.Errorf("title = %q, want the kind's fixed phrase", message.Data[KeyTitle])
	}
	if message.Data[KeyBody] != "Studio" {
		t.Errorf("body = %q, want only the backend's display name", message.Data[KeyBody])
	}
}

// A retraction is held to the narrower contract `notify.ValidateSend`
// states: an id and a kind, nothing to render and nowhere to go.
func TestARetractionCarriesOnlyItsIdAndKind(t *testing.T) {
	message, err := MessageFor(notify.Send{
		ID: "thread:t-1", Kind: notify.KindTurnComplete, Retract: true,
	}, "device-token", "Studio")
	if err != nil {
		t.Fatalf("MessageFor: %v", err)
	}
	want := map[string]string{
		KeyID:      "thread:t-1",
		KeyKind:    "turn-complete",
		KeyRetract: RetractValue,
	}
	for key, value := range want {
		if message.Data[key] != value {
			t.Errorf("data[%q] = %q, want %q", key, message.Data[key], value)
		}
	}
	if len(message.Data) != len(want) {
		t.Fatalf("a retraction carried %v, want exactly %v", message.Data, want)
	}
	if message.Tag != "thread:t-1" {
		t.Errorf("Tag = %q: a retraction must collapse onto the send it withdraws", message.Tag)
	}
}

func TestMessageForRefusesWhatNoPresenterShouldAct(t *testing.T) {
	valid := notify.Send{
		ID: "thread:t-1", Kind: notify.KindTurnComplete, Title: "T", Body: "B",
		Target: notify.Target{Kind: "thread", ThreadID: "t-1"},
	}
	if _, err := MessageFor(valid, "", "Studio"); err == nil {
		t.Error("a message with no registration token was accepted")
	}
	if _, err := MessageFor(notify.Send{Kind: notify.KindTurnComplete}, "tok", ""); err == nil {
		t.Error("a send with no id was accepted; ValidateSend must run first")
	}
	if _, err := MessageFor(notify.Send{ID: "x", Kind: "invented"}, "tok", ""); err == nil {
		t.Error("a send naming an undeclared kind was accepted")
	}
}
