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
	tokens, ok := credentialTokens(data)
	if !ok {
		return "", false
	}
	if id := idTokenAuthClaims(tokens.IDToken).ChatGPTAccountID; id != "" {
		return id, true
	}
	if id := strings.TrimSpace(tokens.AccountID); id != "" {
		return id, true
	}
	return "", false
}

// CredentialLoginID identifies the login a Codex `auth.json` holds: the
// ChatGPT user together with the workspace CredentialOrgID reports. A
// workspace alone is shared by every member of a team plan. The user is
// `chatgpt_user_id`, or `user_id` as Codex falls back to it in
// login/src/token_data.rs; Codex treats both as optional, so a token with
// neither is named by its email, the identity AO's saved accounts match
// on. Each kind is tagged, so a credential named one way never matches
// one named another. No user and no email, or no workspace, reports
// ("", false), which matches nothing.
func CredentialLoginID(data []byte) (string, bool) {
	tokens, ok := credentialTokens(data)
	if !ok {
		return "", false
	}
	claims := idTokenAuthClaims(tokens.IDToken)
	var user string
	switch {
	case claims.ChatGPTUserID != "":
		user = "user:" + claims.ChatGPTUserID
	case claims.UserID != "":
		user = "user:" + claims.UserID
	case claims.Email != "":
		user = "email:" + strings.ToLower(claims.Email)
	}
	org, ok := CredentialOrgID(data)
	if user == "" || !ok {
		return "", false
	}
	return user + "\x00" + org, true
}

type credentialTokenFields struct {
	IDToken   string `json:"id_token"`
	AccountID string `json:"account_id"`
}

func credentialTokens(data []byte) (credentialTokenFields, bool) {
	var authFile struct {
		Tokens *credentialTokenFields `json:"tokens"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil || authFile.Tokens == nil {
		return credentialTokenFields{}, false
	}
	return *authFile.Tokens, true
}

// idTokenAuthClaimSet is the `https://api.openai.com/auth` namespace of a
// Codex id_token, trimmed, plus the email Codex reads beside it: the
// top-level claim, else the `https://api.openai.com/profile` one.
type idTokenAuthClaimSet struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	UserID           string `json:"user_id"`
	Email            string `json:"-"`
}

// maxIDTokenBytes bounds the JWT this parser will look at. A real Codex
// id_token is a few KiB; the cap only refuses adversarially large blobs
// before the base64 decode allocates payload-sized buffers.
const maxIDTokenBytes = 64 << 10

// idTokenAuthClaims decodes the payload segment of a JWT and returns its
// auth namespace, zero when the token, payload, or namespace is absent or
// malformed.
func idTokenAuthClaims(jwt string) idTokenAuthClaimSet {
	if len(jwt) > maxIDTokenBytes {
		return idTokenAuthClaimSet{}
	}
	// SplitN with one spare slot: a well-formed JWT has exactly 3 segments,
	// and anything with more is malformed — no need to allocate a slice
	// entry per dot to find that out.
	parts := strings.SplitN(jwt, ".", 4)
	if len(parts) != 3 {
		return idTokenAuthClaimSet{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenAuthClaimSet{}
	}
	var claims struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth idTokenAuthClaimSet `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return idTokenAuthClaimSet{}
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		email = strings.TrimSpace(claims.Profile.Email)
	}
	return idTokenAuthClaimSet{
		ChatGPTAccountID: strings.TrimSpace(claims.Auth.ChatGPTAccountID),
		ChatGPTUserID:    strings.TrimSpace(claims.Auth.ChatGPTUserID),
		UserID:           strings.TrimSpace(claims.Auth.UserID),
		Email:            email,
	}
}
