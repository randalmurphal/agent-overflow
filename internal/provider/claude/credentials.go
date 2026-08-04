package claude

import (
	"encoding/json"
	"strings"
)

// CredentialsSignedOut reports whether native credential bytes hold the blank
// husk claude >= 2.1.219 leaves behind after a failed startup token refresh:
// the claudeAiOauth object still present, but accessToken and refreshToken
// both empty. Spike-verified 2026-08-03: on invalid_grant the CLI rewrites
// .credentials.json in place to accessToken:"" refreshToken:"" expiresAt:0
// rather than deleting it, and its zero-turn probe then reports success with
// tokenSource:"none".
//
// A husk is a sign-out, not a credential — adopting it as an external login
// would overwrite a saved account slot with unusable bytes. Bytes that do not
// parse, or that carry no claudeAiOauth object at all (an API-key setup, a
// foreign shape), are NOT a husk; those flow through the normal probe paths.
// A half-empty pair is not one either: an empty access token next to a live
// refresh token is refreshable, so it must keep reconciling.
func CredentialsSignedOut(data []byte) bool {
	var credentials struct {
		ClaudeAIOauth *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return false
	}
	oauth := credentials.ClaudeAIOauth
	if oauth == nil {
		return false
	}
	return strings.TrimSpace(oauth.AccessToken) == "" &&
		strings.TrimSpace(oauth.RefreshToken) == ""
}
