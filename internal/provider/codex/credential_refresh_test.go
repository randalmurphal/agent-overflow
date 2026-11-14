package codex

import (
	"testing"
	"time"
)

// The fingerprint names the refresh token, so a rewrite that keeps it (an
// agent identity record) keeps the fingerprint and a rotation changes it.
func TestCredentialRefreshTokenFingerprint(t *testing.T) {
	base, ok := CredentialRefreshTokenFingerprint([]byte(`{"tokens":{"refresh_token":"rt-1","access_token":"a"},"last_refresh":"2026-10-01T00:00:00Z"}`))
	if !ok || len(base) != 64 {
		t.Fatalf("fingerprint = %q, %v", base, ok)
	}
	rewritten, _ := CredentialRefreshTokenFingerprint([]byte(`{"tokens":{"refresh_token":"rt-1","access_token":"a"},"last_refresh":"2026-10-01T00:00:00Z","agent_identity":{"id":"x"}}`))
	if rewritten != base {
		t.Fatal("a rewrite that kept the refresh token changed its fingerprint")
	}
	rotated, _ := CredentialRefreshTokenFingerprint([]byte(`{"tokens":{"refresh_token":"rt-2","access_token":"b"}}`))
	if rotated == base {
		t.Fatal("a rotation kept the fingerprint")
	}
	for _, data := range []string{`{"auth_mode":"apikey","OPENAI_API_KEY":"sk"}`, `{"tokens":{"refresh_token":" "}}`, `not json`} {
		if fingerprint, ok := CredentialRefreshTokenFingerprint([]byte(data)); ok {
			t.Fatalf("%s has fingerprint %q", data, fingerprint)
		}
	}
}

func TestCredentialLastRefresh(t *testing.T) {
	want := time.Date(2026, 10, 15, 2, 48, 19, 48123456, time.UTC)
	for _, tc := range []struct {
		name string
		data string
		ok   bool
	}{
		{name: "chatgpt login", data: `{"auth_mode":"chatgpt","tokens":{"access_token":"a"},"last_refresh":"2026-10-15T02:48:19.048123456Z"}`, ok: true},
		{name: "api key login", data: `{"auth_mode":"apikey","OPENAI_API_KEY":"k","last_refresh":null}`},
		{name: "field absent", data: `{"tokens":{}}`},
		{name: "not json", data: `not json`},
		{name: "not a timestamp", data: `{"last_refresh":"yesterday"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CredentialLastRefresh([]byte(tc.data))
			if ok != tc.ok || (ok && !got.Equal(want)) {
				t.Fatalf("CredentialLastRefresh() = %v, %v; want %v, %v", got, ok, want, tc.ok)
			}
		})
	}
}
