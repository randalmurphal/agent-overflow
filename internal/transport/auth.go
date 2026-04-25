package transport

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

// tokenByteLen is the entropy budget for an ephemeral session token.
// 32 bytes -> 256 bits of randomness, base64url-encoded to 43 ASCII chars.
// That is comfortably more than necessary to defeat online guessing on a
// loopback or LAN bind, and comfortably short enough to fit in a query
// string without scaring off curl users.
const tokenByteLen = 32

// ErrEmptyToken is returned by ConstantTimeEqual when either side is
// empty. Empty tokens never match anything — including each other —
// because an unset server token must not auto-authorise an unset client.
var ErrEmptyToken = errors.New("transport: empty token")

// NewToken returns a freshly-generated ephemeral session token. Caller
// embeds it in the URL handed to the webview / shared with a remote
// client; no persistence by default. Per-launch tokens mean a stale
// bookmarked URL can't reach a new server boot.
func NewToken() (string, error) {
	buf := make([]byte, tokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ConstantTimeEqual compares two tokens in constant time. Returns
// ErrEmptyToken if either side is empty so the caller surfaces a
// distinct "not configured" error, distinct from "wrong token". Returns
// nil on match, a generic comparison error on mismatch.
func ConstantTimeEqual(server, supplied string) error {
	if server == "" || supplied == "" {
		return ErrEmptyToken
	}
	a := []byte(server)
	b := []byte(supplied)
	if subtle.ConstantTimeCompare(a, b) != 1 {
		return errors.New("transport: token mismatch")
	}
	return nil
}
