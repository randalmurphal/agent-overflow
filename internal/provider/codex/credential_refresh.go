package codex

import (
	"encoding/json"
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
