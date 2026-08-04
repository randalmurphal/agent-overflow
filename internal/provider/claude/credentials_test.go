package claude

import "testing"

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
