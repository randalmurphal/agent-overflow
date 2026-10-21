package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// CredentialLastRefresh reports the `last_refresh` timestamp of a Codex
// `auth.json`. Codex stamps it on every sign-in and every persisted token
// refresh (codex-rs login/src/auth/manager.rs persist_tokens), so it only
// moves forward along one login's rotation chain and a later sign-in lands
// after it. Auth modes that never refresh (API key, Bedrock) leave it null,
// and they, like bytes that don't parse, report ok=false.
func CredentialLastRefresh(data []byte) (time.Time, bool) {
	var authFile struct {
		LastRefresh *time.Time `json:"last_refresh"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil || authFile.LastRefresh == nil || authFile.LastRefresh.IsZero() {
		return time.Time{}, false
	}
	return *authFile.LastRefresh, true
}

// CredentialRefreshTokenFingerprint is a SHA-256 of the refresh token a Codex
// `auth.json` holds, hex encoded: the material a rotation consumes. Codex
// rewrites the file for other reasons too (an agent identity record, for
// one) without touching the token, so the token rather than the file is what
// identifies a credential across those rewrites. ok is false when there is
// no refresh token (API-key and other non-refreshing modes).
func CredentialRefreshTokenFingerprint(data []byte) (string, bool) {
	var authFile struct {
		Tokens *struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil || authFile.Tokens == nil {
		return "", false
	}
	token := strings.TrimSpace(authFile.Tokens.RefreshToken)
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), true
}
