package claude

import (
	"fmt"
	"testing"
	"time"
)

func TestCredentialsSignedOut(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		// The exact shape claude 2.1.219 writes after a failed startup
		// refresh (spike 2026-08-03).
		{
			"blanked husk",
			`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":[],"subscriptionType":null}}`,
			true,
		},
		{"whitespace tokens", `{"claudeAiOauth":{"accessToken":"  ","refreshToken":"\t"}}`, true},
		{"tokens absent but object present", `{"claudeAiOauth":{"expiresAt":0}}`, true},

		// A live refresh token can still rotate; an access token can still
		// authenticate. Neither half-empty pair is a sign-out.
		{"refresh token survives", `{"claudeAiOauth":{"accessToken":"","refreshToken":"live"}}`, false},
		{"access token survives", `{"claudeAiOauth":{"accessToken":"live","refreshToken":""}}`, false},
		{"full pair", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"}}`, false},

		// Not claude-oauth shaped: no claim either way — callers' normal
		// probe paths decide.
		{"no oauth object", `{"somethingElse":true}`, false},
		{"empty object", `{}`, false},
		{"codex shape", `{"tokens":{"access_token":"x"}}`, false},
		{"invalid json", `{"claudeAiOauth":`, false},
		{"empty bytes", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CredentialsSignedOut([]byte(tc.data)); got != tc.want {
				t.Fatalf("CredentialsSignedOut(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestCredentialExpired(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	expired := now.Add(-time.Second).UnixMilli()
	live := now.Add(8 * time.Hour).UnixMilli()

	cases := []struct {
		name string
		data string
		want bool
	}{
		{"elapsed", fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","expiresAt":%d}}`, expired), true},
		// The boundary belongs to the expired side: a token whose lifetime
		// ends exactly now buys no request.
		{"exactly now", fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","expiresAt":%d}}`, now.UnixMilli()), true},
		{"still live", fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","expiresAt":%d}}`, live), false},

		// No readable expiry is no claim — the HTTP probe decides.
		{"zero expiry", `{"claudeAiOauth":{"accessToken":"a","expiresAt":0}}`, false},
		{"negative expiry", `{"claudeAiOauth":{"accessToken":"a","expiresAt":-1}}`, false},
		{"expiry absent", `{"claudeAiOauth":{"accessToken":"a"}}`, false},
		{"no oauth object", `{"somethingElse":true}`, false},
		{"codex shape", `{"tokens":{"access_token":"x"}}`, false},
		{"invalid json", `{"claudeAiOauth":`, false},
		{"empty bytes", ``, false},

		// Seconds mistaken for milliseconds would read every live token as
		// expired; the unit is pinned by a value that is only in the future
		// when read as milliseconds.
		{"milliseconds not seconds", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1800000000001}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CredentialExpired([]byte(tc.data), now); got != tc.want {
				t.Fatalf("CredentialExpired(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}
