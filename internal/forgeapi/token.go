package forgeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"agent-overflow/internal/secret"
)

// AuthHeader is how a token is presented to the forge.
type AuthHeader int

const (
	// AuthBearer sends "Authorization: Bearer <token>": every GitHub token
	// and a GitLab OAuth2 login.
	AuthBearer AuthHeader = iota
	// AuthPrivateToken sends "Private-Token: <token>": a GitLab personal
	// access token, as glab sends it.
	AuthPrivateToken
)

// Token holds one host's bearer secret. Only authorize reads the bytes;
// every formatter and marshaler prints secret.Redacted, including when a
// Token is reached through an unexported field (see package secret).
//
// A Token is not safe for concurrent use: the credential holder serializes
// authorize against Zero.
type Token struct {
	secret *secret.Bytes
	header AuthHeader
}

// newToken copies b into a new holder.
func newToken(b []byte, header AuthHeader) *Token {
	return &Token{secret: secret.New(b), header: header}
}

// authorize sets the token's header on req. A zeroed token sets nothing,
// so the forge answers 401 and the caller re-reads the credential.
func (t *Token) authorize(req *http.Request) {
	if t == nil || t.secret.Len() == 0 {
		return
	}
	value := string(t.secret.Reveal())
	switch t.header {
	case AuthPrivateToken:
		req.Header.Set("Private-Token", value)
	default:
		req.Header.Set("Authorization", "Bearer "+value)
	}
}

// Zero overwrites the secret and drops it.
func (t *Token) Zero() {
	if t == nil {
		return
	}
	t.secret.Zero()
}

func (Token) String() string   { return secret.Redacted }
func (Token) GoString() string { return secret.Redacted }

// Format prints secret.Redacted for every verb and flag, %q and %x included.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(secret.Redacted)) }

func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + secret.Redacted + `"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte(secret.Redacted), nil }

// fingerprint is the hex sha256 of a token: the key of its rate gates and
// ETag entries, so the secret never appears in a map key, a log or a test
// name.
func fingerprint(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])
}
