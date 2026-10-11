package forgeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
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

const redacted = "<redacted>"

// Token holds one host's bearer secret. Only authorize reads the bytes;
// every formatter and marshaler prints "<redacted>". fmt cannot call these
// methods on an unexported struct field, so a Token is never stored by
// value in a struct that might be printed; this package keeps it behind a
// pointer in its credential holder.
//
// A Token is not safe for concurrent use: the credential holder serializes
// authorize against Zero.
type Token struct {
	b      []byte
	header AuthHeader
}

// newToken copies secret into a new holder.
func newToken(secret []byte, header AuthHeader) *Token {
	return &Token{b: append([]byte(nil), secret...), header: header}
}

// authorize sets the token's header on req. A zeroed token sets nothing,
// so the forge answers 401 and the caller re-reads the credential.
func (t *Token) authorize(req *http.Request) {
	if t == nil || len(t.b) == 0 {
		return
	}
	switch t.header {
	case AuthPrivateToken:
		req.Header.Set("Private-Token", string(t.b))
	default:
		req.Header.Set("Authorization", "Bearer "+string(t.b))
	}
}

// Zero overwrites the secret and drops it.
func (t *Token) Zero() {
	if t == nil {
		return
	}
	clear(t.b)
	t.b = nil
}

func (Token) String() string   { return redacted }
func (Token) GoString() string { return redacted }

// Format prints "<redacted>" for every verb and flag, %q and %x included.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// fingerprint is the hex sha256 of a token: the key of its rate gates and
// ETag entries, so the secret never appears in a map key, a log or a test
// name.
func fingerprint(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])
}
