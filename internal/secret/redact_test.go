package secret

import (
	"errors"
	"strings"
	"testing"
)

// A token is redacted from an error's message raw and as a URL query
// carries it.
func TestRedactErrorRemovesTheTokenInEveryForm(t *testing.T) {
	token := "secret+/token"
	got := RedactError(errors.New("GET ws://127.0.0.1/ws?token=secret%2B%2Ftoken: refused, secret+/token"), token)
	if strings.Contains(got, token) || strings.Contains(got, "secret%2B%2Ftoken") {
		t.Fatalf("redacted error leaked the token: %q", got)
	}
	if want := "GET ws://127.0.0.1/ws?token=" + Redacted + ": refused, " + Redacted; got != want {
		t.Fatalf("RedactError = %q, want %q", got, want)
	}
	if got := RedactError(nil, token); got != "unknown error" {
		t.Fatalf("RedactError(nil) = %q", got)
	}
	if got := RedactError(errors.New("refused"), ""); got != "refused" {
		t.Fatalf("RedactError with no token = %q", got)
	}
}
