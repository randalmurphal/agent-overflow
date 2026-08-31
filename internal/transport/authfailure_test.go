package transport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAuthFailureCarriesTheReasonOnTheWire(t *testing.T) {
	frame := ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-9",
		Error: AuthFailure("outside_time_window"),
	}
	buf, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var out ServerFrame
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil {
		t.Fatal("error envelope dropped")
	}
	if out.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("code is %q, want %q", out.Error.Code, ErrCodeAuthFailed)
	}
	if out.Error.Reason != "outside_time_window" {
		t.Fatalf("reason is %q; the client cannot pick a hint without it", out.Error.Reason)
	}
}

// TestReasonIsOmittedOnEveryOtherError — a client reads the field as "was
// this a credential refusal, and why". A reason on a method failure would
// make that question ambiguous, and the hint module would translate a
// value that describes nothing.
func TestReasonIsOmittedOnEveryOtherError(t *testing.T) {
	buf, err := json.Marshal(ServerFrame{
		Type:  frameTypeRPC,
		ID:    "req-10",
		Error: &FrameError{Code: ErrCodeMethodError, Message: "method failed (id: abc123)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(buf), `"reason"`) {
		t.Fatalf("an ordinary method error carried a reason field: %s", buf)
	}
}

// TestAuthFailureMessageCarriesNothingSpecific — the message is redacted
// for non-loopback callers, so a refusal that put its meaning there would
// be readable on the desktop and blank everywhere else. The code and the
// reason are what travel.
func TestAuthFailureMessageCarriesNothingSpecific(t *testing.T) {
	for _, reason := range []string{"revoked_session", "expired_session", "key_mismatch"} {
		if got := AuthFailure(reason).Message; got != "not authorized" {
			t.Fatalf("AuthFailure(%q) message is %q; refusals must not differ in prose",
				reason, got)
		}
	}
}
