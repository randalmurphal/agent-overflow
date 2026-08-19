package claude

import (
	"encoding/json"
	"strings"
	"time"
)

// CredentialsSignedOut reports whether native credential bytes hold the blank
// husk claude >= 2.1.219 leaves behind after a failed startup token refresh:
// the claudeAiOauth object still present, but accessToken and refreshToken
// both empty. Spike-verified 2026-08-03: on invalid_grant the CLI rewrites
// .credentials.json in place to accessToken:"" refreshToken:"" expiresAt:0
// rather than deleting it, and its zero-turn probe then reports success with
// tokenSource:"none".
//
// A husk is a sign-out, not a credential. This predicate started as an
// adoption filter (don't overwrite a saved slot with unusable bytes) and now
// gates every durable credential write: it backs the provideraccounts
// sign-out detector, so a true verdict refuses slot writes, canonical
// activation, switches, and ephemeral seeding, and surfaces the account as
// needing login. That makes a false positive expensive — bytes wrongly
// called a husk would have their rotation DROPPED at the write layer, which
// is itself a bricked login. Widen this predicate only with spike evidence.
//
// Bytes that do not parse, or that carry no claudeAiOauth object at all (an
// API-key setup, a foreign shape), are NOT a husk; those flow through the
// normal probe paths. A half-empty pair is not one either: an empty access
// token next to a live refresh token is refreshable, so it must keep
// reconciling.
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

// CredentialExpiresAt reads `claudeAiOauth.expiresAt` — epoch MILLISECONDS —
// out of native credential bytes. ok is false when the bytes do not parse,
// carry no claudeAiOauth object, or carry no positive expiry (an API-key
// setup, a foreign shape, the sign-out husk).
//
// It doubles as this provider's ROTATION-CHAIN ORDER. Claude issues fixed-TTL
// access tokens (`expires_in` 28800 — 8h) and only ever refreshes within five
// minutes of expiry, so every mint lands strictly further out than the one it
// replaces: a larger expiresAt means later in the same account's chain. That
// makes it the signal a saved slot uses to refuse moving backwards onto a
// refresh token the server has already retired.
func CredentialExpiresAt(data []byte) (int64, bool) {
	var credentials struct {
		ClaudeAIOauth *struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return 0, false
	}
	oauth := credentials.ClaudeAIOauth
	if oauth == nil || oauth.ExpiresAt <= 0 {
		return 0, false
	}
	return oauth.ExpiresAt, true
}

// CredentialExpired reports whether native credential bytes carry an access
// token whose lifetime has already elapsed, answering "the usage endpoint
// would reject this bearer" without sending it.
//
// Callers use it to skip a request that can only fail, never as a claim about
// the credential's health: an absent, zero, or unparseable expiry answers
// false and falls through to the HTTP probe, which is authoritative either way.
//
// Passing a now shifted into the future asks the other question the same field
// answers — "would the CLI refresh this on startup" — since Claude refreshes
// proactively inside a buffer rather than waiting for a 401.
func CredentialExpired(data []byte, now time.Time) bool {
	expiresAt, ok := CredentialExpiresAt(data)
	if !ok {
		return false
	}
	return !time.UnixMilli(expiresAt).After(now)
}
