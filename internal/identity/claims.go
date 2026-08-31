package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"
)

// Session claims: the signed half of a credential.
//
// Wire form is `ao1.<payload>.<mac>`, both parts base64url without
// padding, 103 bytes total. Deliberately not JWT and deliberately not
// JSON:
//
//   - Fixed binary layout parses by slicing and two binary.BigEndian
//     reads. There is no field order to disagree about, no unknown-field
//     question, and no parser to feed untrusted length prefixes.
//   - No algorithm field. The only algorithm is HMAC-SHA256, chosen by
//     this code, so no presentation can propose a different one.
//
// The payload carries only what is needed to FIND the session and bound
// its lifetime. User, device, binding class, and scopes live on the
// database row and nowhere else, so those facts have exactly one source
// and a claims/row disagreement is not a state that can exist.
const (
	// claimsPrefix versions the whole encoding, including the MAC input.
	// A second version would be a different prefix and a different parse,
	// never a flag inside the payload.
	claimsPrefix = "ao1."

	// Payload layout, big-endian:
	//	[0]      format version
	//	[1:9]    signing key id (8 bytes)
	//	[9:25]   session id (uuid, 16 bytes)
	//	[25:33]  issued at, Unix milliseconds
	//	[33:41]  expires at, Unix milliseconds
	claimsVersion     = 1
	claimsKeyIDLen    = 8
	claimsPayloadLen  = 1 + claimsKeyIDLen + 16 + 8 + 8
	claimsMACLen      = sha256.Size
	claimsKeyIDOffset = 1
	claimsSIDOffset   = claimsKeyIDOffset + claimsKeyIDLen
	claimsIatOffset   = claimsSIDOffset + 16
	claimsExpOffset   = claimsIatOffset + 8
)

// macDomain separates this MAC's inputs from any other use of the same
// secret. It is prepended to the MAC input, never to the payload, so it
// costs no wire bytes.
const macDomain = "agent-overflow/session-claims/v1\x00"

// Claims is the decoded payload of a session credential.
type Claims struct {
	// KeyID names the signing key, as the 16 hex characters that spell the
	// `signing_keys.id` of the row holding the secret.
	KeyID string
	// SessionID is the session row's uuid.
	SessionID string
	// IssuedAt and ExpiresAt are Unix milliseconds.
	IssuedAt  int64
	ExpiresAt int64
}

// verifiedClaims is Claims whose signature has been checked.
//
// This type is the ordering guarantee, not a comment about it. The time
// window is only reachable through withinWindow, which is a method on
// THIS type, and the only constructor is checkSignature's success path.
// So there is no arrangement of calls — including one a later edit
// introduces — in which a proof that failed to verify is reported as a
// clock problem. That misreport is the specific failure this shape
// exists to make unrepresentable (docs/specs/remote-access.md §4).
type verifiedClaims struct {
	claims Claims
}

// maxFutureSkew is how far ahead of this host's clock a verified proof may
// claim to have been issued. Two minutes covers an NTP correction landing
// after boot and a resumed VM; beyond it the timestamps genuinely disagree
// and saying so is more useful than admitting the credential.
const maxFutureSkewMillis int64 = 2 * 60 * 1000

// withinWindow checks the signed validity window against now (Unix
// milliseconds). The two failures are kept apart because their remedies
// are: a future-dated proof is a clock to fix, an expired one is a
// credential to renew.
func (v verifiedClaims) withinWindow(now int64) Reason {
	if v.claims.IssuedAt-now > maxFutureSkewMillis {
		return ReasonOutsideTimeWindow
	}
	if now >= v.claims.ExpiresAt {
		return ReasonExpiredSession
	}
	return ReasonNone
}

// signClaims renders a credential. secret is the signing key's bytes and
// backendID binds the MAC to this backend.
//
// Binding to the backend id is what makes a database restored under a new
// identity refuse every session it imported, which is the re-pairing
// recovery the spec already states (§12) rather than a second mechanism.
func signClaims(claims Claims, secret []byte, backendID string) (string, error) {
	payload, err := encodeClaims(claims)
	if err != nil {
		return "", err
	}
	mac := claimsMAC(payload[:], secret, backendID)
	out := make([]byte, 0, len(claimsPrefix)+
		base64.RawURLEncoding.EncodedLen(claimsPayloadLen)+1+
		base64.RawURLEncoding.EncodedLen(claimsMACLen))
	out = append(out, claimsPrefix...)
	out = base64.RawURLEncoding.AppendEncode(out, payload[:])
	out = append(out, '.')
	out = base64.RawURLEncoding.AppendEncode(out, mac[:])
	return string(out), nil
}

// encodeClaims lays the payload out. Returns an array rather than a slice
// so the buffer stays on the caller's stack.
func encodeClaims(claims Claims) ([claimsPayloadLen]byte, error) {
	var payload [claimsPayloadLen]byte
	keyID, err := decodeKeyID(claims.KeyID)
	if err != nil {
		return payload, err
	}
	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return payload, fmt.Errorf("identity: session id %q is not a uuid: %w", claims.SessionID, err)
	}
	payload[0] = claimsVersion
	copy(payload[claimsKeyIDOffset:], keyID[:])
	copy(payload[claimsSIDOffset:], sessionID[:])
	binary.BigEndian.PutUint64(payload[claimsIatOffset:], uint64(claims.IssuedAt))
	binary.BigEndian.PutUint64(payload[claimsExpOffset:], uint64(claims.ExpiresAt))
	return payload, nil
}

// parseClaims decodes a presentation's structure. It learns nothing about
// authenticity — every field it returns is caller-supplied until the MAC
// is checked, which is why it is unexported and its result is not Claims
// anyone outside this file can act on.
func parseClaims(credential string) (Claims, [claimsPayloadLen]byte, [claimsMACLen]byte, Reason) {
	var payload [claimsPayloadLen]byte
	var mac [claimsMACLen]byte

	const encodedPayloadLen = (claimsPayloadLen*8 + 5) / 6
	const encodedMACLen = (claimsMACLen*8 + 5) / 6
	const wantLen = len(claimsPrefix) + encodedPayloadLen + 1 + encodedMACLen

	if len(credential) != wantLen ||
		credential[:len(claimsPrefix)] != claimsPrefix ||
		credential[len(claimsPrefix)+encodedPayloadLen] != '.' {
		return Claims{}, payload, mac, ReasonMalformedProof
	}
	payloadText := credential[len(claimsPrefix) : len(claimsPrefix)+encodedPayloadLen]
	macText := credential[len(claimsPrefix)+encodedPayloadLen+1:]

	if n, err := base64.RawURLEncoding.Decode(payload[:], []byte(payloadText)); err != nil || n != claimsPayloadLen {
		return Claims{}, payload, mac, ReasonMalformedProof
	}
	if n, err := base64.RawURLEncoding.Decode(mac[:], []byte(macText)); err != nil || n != claimsMACLen {
		return Claims{}, payload, mac, ReasonMalformedProof
	}
	if payload[0] != claimsVersion {
		return Claims{}, payload, mac, ReasonMalformedProof
	}
	var sessionID uuid.UUID
	copy(sessionID[:], payload[claimsSIDOffset:claimsIatOffset])
	return Claims{
		KeyID:     encodeKeyID(payload[claimsKeyIDOffset:claimsSIDOffset]),
		SessionID: sessionID.String(),
		IssuedAt:  int64(binary.BigEndian.Uint64(payload[claimsIatOffset:claimsExpOffset])),
		ExpiresAt: int64(binary.BigEndian.Uint64(payload[claimsExpOffset:])),
	}, payload, mac, ReasonNone
}

// claimsMAC computes the tag. The domain separator and the backend id
// precede the payload, so a secret reused for anything else and a payload
// minted by a different backend both fail here rather than somewhere
// subtler.
func claimsMAC(payload []byte, secret []byte, backendID string) [claimsMACLen]byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(macDomain))
	mac.Write([]byte(backendID))
	mac.Write([]byte{0})
	mac.Write(payload)
	var out [claimsMACLen]byte
	mac.Sum(out[:0])
	return out
}

// encodeKeyID / decodeKeyID render a signing key's 8 identifying bytes as
// the 16 lowercase hex characters stored in `signing_keys.id`. Written out
// rather than calling encoding/hex so both directions are one fixed-size
// loop with no allocation beyond the result string.
const hexDigits = "0123456789abcdef"

func encodeKeyID(raw []byte) string {
	var out [claimsKeyIDLen * 2]byte
	for i, b := range raw[:claimsKeyIDLen] {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out[:])
}

func decodeKeyID(id string) ([claimsKeyIDLen]byte, error) {
	var out [claimsKeyIDLen]byte
	if len(id) != claimsKeyIDLen*2 {
		return out, fmt.Errorf("identity: signing key id %q is not %d hex characters", id, claimsKeyIDLen*2)
	}
	for i := range out {
		hi, hiOK := hexValue(id[i*2])
		lo, loOK := hexValue(id[i*2+1])
		if !hiOK || !loOK {
			return out, fmt.Errorf("identity: signing key id %q is not hex", id)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}
