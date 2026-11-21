package harnessrpc

import (
	"encoding/json"
	"testing"
)

// TestHarnessRequestWaitsNamesEveryTokenAsked: the receiver hands the trimmed
// tokens to the host and answers each one, held or not, as a JSON object a
// spec reads by token. The registry itself is tested in internal/app
// (TestHarnessRequestWaitsReadsTheWaitRegistry).
func TestHarnessRequestWaitsNamesEveryTokenAsked(t *testing.T) {
	h, host := newHarnessTestHost(t)
	host.requestWaits = map[string]bool{"held": true}
	got, err := h.HarnessRequestWaits([]string{" held ", "free"})
	if err != nil {
		t.Fatalf("HarnessRequestWaits: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"free":false,"held":true}` {
		t.Fatalf("result = %s, want every token named with whether a call holds it", encoded)
	}
}

// TestHarnessRequestWaitsRefusesNoTokenAndABlankOne: an empty question has
// no answer, and a blank token is a spec bug, not a token nobody holds.
func TestHarnessRequestWaitsRefusesNoTokenAndABlankOne(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessRequestWaits(nil); err == nil {
		t.Fatal("no tokens was answered")
	}
	if _, err := h.HarnessRequestWaits([]string{"held", "  "}); err == nil {
		t.Fatal("a blank token was answered")
	}
}
