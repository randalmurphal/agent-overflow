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

// RefreshTokenExpiresAt reads `claudeAiOauth.refreshTokenExpiresAt` — epoch
// MILLISECONDS — out of native credential bytes. ok is false when the bytes do
// not parse, carry no claudeAiOauth object, or carry no positive value (an
// API-key setup, a foreign shape, the sign-out husk, a credential written
// before the CLI recorded the field).
//
// This is the OAuth SESSION's own deadline, and it is a different clock from
// CredentialExpiresAt: that one bounds the eight-hour access token the refresh
// chain keeps reissuing, this one bounds how long the chain may be reissued at
// all. Spike-verified against 2.1.257 and the four logins on the development
// host: the CLI sets it at sign-in from the token response's
// `refresh_token_expires_in` (defaulting to 30 days when the server omits it)
// and a refresh does NOT extend it — the CLI keeps the on-disk value unless
// the server sends a new one, and the server does not, so an account refreshed
// hours ago still carries its sign-in-day expiry to the second.
//
// Past it the next refresh answers invalid_grant and the CLI blanks the
// credential to the sign-out husk (see CredentialsSignedOut). That makes this
// field the ONE signal that can see a login's death coming, and the reason
// Agent Overflow reads it: a refresh attempted after this moment does not fail
// harmlessly, it destroys the credential it was trying to renew.
func RefreshTokenExpiresAt(data []byte) (time.Time, bool) {
	var credentials struct {
		ClaudeAIOauth *struct {
			RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return time.Time{}, false
	}
	oauth := credentials.ClaudeAIOauth
	if oauth == nil || oauth.RefreshTokenExpiresAt <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(oauth.RefreshTokenExpiresAt), true
}

// RefreshTokenExpired reports that this login's OAuth session is over: the
// refresh token's own lifetime has elapsed, so the next refresh answers
// invalid_grant and the CLI blanks the credential in place.
//
// Bytes carrying no refresh-token expiry answer false. Absent is UNKNOWN, not
// expired: credentials written by older CLIs (and every Codex credential) have
// no such field, and reading their silence as death would sign working
// accounts out.
func RefreshTokenExpired(data []byte, now time.Time) bool {
	expiresAt, ok := RefreshTokenExpiresAt(data)
	if !ok {
		return false
	}
	return !expiresAt.After(now)
}
