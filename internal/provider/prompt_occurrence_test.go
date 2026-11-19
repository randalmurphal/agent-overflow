package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// A prompt's occurrence reaches a client as a string, past any JSON number
// limit, and comes back on its answer as the same value. A client that
// sends none answers with zero, and the field is absent when there is none.
func TestAPromptOccurrenceRoundTripsAsAString(t *testing.T) {
	const big = PromptOccurrence(1<<63 + 7)
	request, err := json.Marshal(ApprovalRequest{RequestID: "r", Occurrence: big, SessionToken: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request), `"occurrence":"9223372036854775815"`) || strings.Contains(string(request), `"s"`) {
		t.Fatalf("approval request JSON = %s, want the occurrence as a string and no session token", request)
	}
	question, err := json.Marshal(UserInputRequest{RequestID: "r", Occurrence: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(question), `"occurrence":"3"`) {
		t.Fatalf("user input request JSON = %s, want the occurrence", question)
	}
	if none, _ := json.Marshal(ApprovalResponse{RequestID: "r"}); strings.Contains(string(none), "occurrence") {
		t.Fatalf("an answer naming no prompt = %s, want no occurrence field", none)
	}

	var approval ApprovalResponse
	if err := json.Unmarshal([]byte(`{"requestId":"r","occurrence":"9223372036854775815","decision":"accept"}`), &approval); err != nil || approval.Occurrence != big {
		t.Fatalf("approval answer occurrence = %v (%v), want %v", approval.Occurrence, err, big)
	}
	var answer UserInputResponse
	if err := json.Unmarshal([]byte(`{"requestId":"r","occurrence":"3"}`), &answer); err != nil || answer.Occurrence != 3 {
		t.Fatalf("user input answer occurrence = %v (%v), want 3", answer.Occurrence, err)
	}
	var legacy ApprovalResponse
	if err := json.Unmarshal([]byte(`{"requestId":"r","decision":"accept"}`), &legacy); err != nil || legacy.Occurrence != 0 {
		t.Fatalf("an older client's answer occurrence = %v (%v), want zero", legacy.Occurrence, err)
	}
	for _, bad := range []string{`"x"`, `"-1"`, `3`} {
		var refused ApprovalResponse
		if err := json.Unmarshal([]byte(`{"requestId":"r","occurrence":`+bad+`}`), &refused); err == nil {
			t.Fatalf("occurrence %s decoded as %v, want an error", bad, refused.Occurrence)
		}
	}
}
