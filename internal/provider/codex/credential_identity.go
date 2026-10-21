package codex

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// CredentialOrgID extracts the ChatGPT workspace/organization identifier
// from the raw bytes of a Codex `auth.json`. This is the identity axis the
// app-server's `account/read` cannot answer: its wire shape carries only
// `{type, email, planType}`, while one email legitimately holds a separate
// login per workspace. Codex itself keys every identity-change check on
// this id (it goes out as the `ChatGPT-Account-Id` header), never on email.
//
// Source order mirrors upstream's own precedence inverted for staleness:
// the `chatgpt_account_id` claim inside `tokens.id_token`'s
// `https://api.openai.com/auth` object is preferred, because a token
// refresh rewrites the id_token but NOT the denormalized top-level
// `tokens.account_id` (codex-rs login/src/auth/manager.rs persist_tokens),
// which therefore serves only as a fallback for a token whose claim is
// absent. The JWT is decoded, never verified — Codex itself only
// base64-decodes the payload segment, and these bytes come from the local
// credential store, not a network peer.
//
// API-key, Bedrock, and PAT auth modes carry no workspace identity on
// disk; they, and any bytes that don't parse, report ("", false). Blank
// means UNKNOWN to every consumer, never "no organization".
func CredentialOrgID(data []byte) (string, bool) {
	var authFile struct {
		Tokens *struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil || authFile.Tokens == nil {
		return "", false
	}
	if id := idTokenChatGPTAccountID(authFile.Tokens.IDToken); id != "" {
		return id, true
	}
	if id := strings.TrimSpace(authFile.Tokens.AccountID); id != "" {
		return id, true
	}
	return "", false
}

// idTokenChatGPTAccountID decodes the payload segment of a JWT and returns
// the `chatgpt_account_id` claim from the `https://api.openai.com/auth`
// namespace, or "" when the token, payload, or claim is absent/malformed.
// maxIDTokenBytes bounds the JWT this parser will look at. A real Codex
// id_token is a few KiB; the cap only refuses adversarially large blobs
// before the base64 decode allocates payload-sized buffers.
const maxIDTokenBytes = 64 << 10

func idTokenChatGPTAccountID(jwt string) string {
	if len(jwt) > maxIDTokenBytes {
		return ""
	}
	// SplitN with one spare slot: a well-formed JWT has exactly 3 segments,
	// and anything with more is malformed — no need to allocate a slice
	// entry per dot to find that out.
	parts := strings.SplitN(jwt, ".", 4)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Auth.ChatGPTAccountID)
}
