package transport

import (
	"context"
	"encoding/json"
	"testing"

	"agent-overflow/internal/dispatchrefusal"
)

// The codes the wording reads are the codes the dispatcher sends.
func TestDispatchRefusalCodesAreTheWireCodes(t *testing.T) {
	if dispatchrefusal.CodeMethodNotFound != ErrCodeMethodNotFound || dispatchrefusal.CodeBadParams != ErrCodeBadParams {
		t.Fatalf("dispatchrefusal codes %q %q, want %q %q", dispatchrefusal.CodeMethodNotFound, dispatchrefusal.CodeBadParams, ErrCodeMethodNotFound, ErrCodeBadParams)
	}
}

// A call this computer does not have, and a call whose arguments do not
// fit what it declares, are refused before any method runs with the
// sentence a person can act on and no detail, so a reader already shipped
// shows that sentence, and a reader tells them from a failure a method
// returned. An argument that does not decode keeps its log reference.
func TestTheDispatcherRefusesWithTheSentenceAndNoDetail(t *testing.T) {
	captureLog(t)
	d := NewDispatcher()
	if _, err := d.Register(&detailApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, fe := d.ResolveForOrigin(0, "AddedLater", true); fe == nil || fe.Code != ErrCodeMethodNotFound || fe.Message != dispatchrefusal.Unsupported || fe.Detail != nil {
		t.Fatalf("missing method = %+v, want method_not_found with the Unsupported sentence and no detail", fe)
	}
	m, fe := d.ResolveForOrigin(0, "TakesCount", true)
	if fe != nil {
		t.Fatalf("resolve TakesCount: %+v", fe)
	}
	for _, tc := range []struct {
		name     string
		params   []json.RawMessage
		detailed bool
	}{
		{"one argument more than declared", []json.RawMessage{json.RawMessage(`1`), json.RawMessage(`2`)}, false},
		{"one argument fewer", nil, false},
		{"more arguments than any call takes", make([]json.RawMessage, MaxRPCParams+1), false},
		{"an argument of another type", []json.RawMessage{json.RawMessage(`"seven"`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fe := d.Invoke(context.Background(), m, tc.params)
			if fe == nil || fe.Code != ErrCodeBadParams || fe.Message != dispatchrefusal.Unreadable || (fe.Detail != nil) != tc.detailed {
				t.Fatalf("refusal = %+v, want bad_params with the Unreadable sentence, detail %v", fe, tc.detailed)
			}
		})
	}
}
