package codex

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func encodeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	segment := base64.RawURLEncoding.EncodeToString
	return segment([]byte(`{"alg":"RS256"}`)) + "." + segment(payload) + "." + segment([]byte("sig"))
}

func TestCredentialOrgIDPrefersIDTokenClaim(t *testing.T) {
	token := encodeIDToken(t, map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "ws-from-claim",
			"chatgpt_user_id":    "user-1",
		},
	})
	data, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      token,
			"access_token":  "at",
			"refresh_token": "rt",
			// Stale denormalized copy: a refresh rewrites the id_token but
			// not this field, so the claim must win when both disagree.
			"account_id": "ws-stale-toplevel",
		},
	})
	id, ok := CredentialOrgID(data)
	if !ok || id != "ws-from-claim" {
		t.Fatalf("CredentialOrgID = %q, %v; want ws-from-claim, true", id, ok)
	}
}

func TestCredentialOrgIDFallsBackToTopLevelAccountID(t *testing.T) {
	// An id_token whose auth namespace lacks the claim.
	token := encodeIDToken(t, map[string]any{"email": "user@example.com"})
	data, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"id_token":   token,
			"account_id": "ws-toplevel",
		},
	})
	id, ok := CredentialOrgID(data)
	if !ok || id != "ws-toplevel" {
		t.Fatalf("CredentialOrgID = %q, %v; want ws-toplevel, true", id, ok)
	}
}

func TestCredentialOrgIDMalformedJWTStillUsesFallback(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"id_token":   "not-a-jwt",
			"account_id": "ws-toplevel",
		},
	})
	id, ok := CredentialOrgID(data)
	if !ok || id != "ws-toplevel" {
		t.Fatalf("CredentialOrgID = %q, %v; want ws-toplevel, true", id, ok)
	}
}

func TestCredentialOrgIDAbsentForNonChatGPTModes(t *testing.T) {
	cases := map[string]string{
		"api key":     `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`,
		"empty":       `{}`,
		"null tokens": `{"tokens":null}`,
		"no ids":      `{"tokens":{"id_token":"","access_token":"at"}}`,
		"not json":    `garbage`,
	}
	for name, raw := range cases {
		if id, ok := CredentialOrgID([]byte(raw)); ok || id != "" {
			t.Fatalf("%s: CredentialOrgID = %q, %v; want blank, false", name, id, ok)
		}
	}
}
