package deviceclient

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// The proof this device presents, minted per request.
//
// The verifier is `internal/identity/deviceproof.go` and it owns the shape;
// everything here is written to satisfy it exactly. Three details decide
// whether a proof verifies at all, and each is easy to get subtly wrong:
//
//   - `htp` is the request PATH, never the full URI. One backend answers
//     on loopback, a LAN address, the WSL launcher's relay and a
//     `--connect` stub's proxy at once, and a client cannot predict which
//     authority its request will be seen under.
//   - `iatMs` is Unix MILLISECONDS. JWT `iat` is seconds; reusing that
//     spelling for a different unit is how a factor-of-1000 bug ships.
//   - The JWK carries the four members RFC 7638 hashes and no others. Go
//     does not emit `ext` or `key_ops` (WebCrypto does), so the risk here
//     is the opposite one: a coordinate rendered short. `x` and `y` are
//     FillBytes'd to the fixed 32-byte width, because a big.Int's minimal
//     encoding would make two spellings of one key hash to two
//     thumbprints and the device would be refused for a key that is
//     correct.
//
// Cost: one SHA-256 and one P-256 sign per credential request. Nothing on
// this path runs per frame.
const (
	// proofType is the exact `typ` the verifier pins.
	proofType = "ao-device-proof+jws"
	// proofAlg is the only signature algorithm, matching the verifier's
	// refusal to negotiate one.
	proofAlg = "ES256"
	// proofCurve and proofKeyType are the JWK's `crv` and `kty`,
	// WebCrypto's spellings and the only pair the verifier accepts.
	proofCurve   = "P-256"
	proofKeyType = "EC"
	// coordBytes is the fixed width of a P-256 affine coordinate
	// (RFC 7518). See the file comment for why it is not the minimal
	// encoding.
	coordBytes = 32
	// proofIDBytes is how much randomness the replay identifier carries.
	// 128 bits, because the backend spends it once and a collision would
	// be a refusal for a proof that was fine.
	proofIDBytes = 16
)

// proofHeader is the JWS protected header.
type proofHeader struct {
	Typ string   `json:"typ"`
	Alg string   `json:"alg"`
	JWK proofJWK `json:"jwk"`
}

// proofJWK is the embedded public key: exactly the four members RFC 7638
// requires for an EC key. The verifier decodes with unknown fields
// refused, so a fifth member is a refusal rather than a value it ignores.
type proofJWK struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// proofPayload is what the signature covers.
type proofPayload struct {
	HTM   string `json:"htm"`
	HTP   string `json:"htp"`
	JTI   string `json:"jti"`
	IatMs int64  `json:"iatMs"`
}

// mintProof signs one proof for one call.
//
// Per request, never cached: the proof is bound to this method and path
// and is spent by the first presentation, so a reused string is refused as
// `proof_replayed` and one carried to another route as `proof_not_bound`.
func mintProof(key *ecdsa.PrivateKey, method, path string, now time.Time) (string, error) {
	if key == nil {
		return "", ErrNoDeviceKey
	}
	id := make([]byte, proofIDBytes)
	if _, err := rand.Read(id); err != nil {
		return "", fmt.Errorf("deviceclient: mint a proof identifier: %w", err)
	}
	header, err := json.Marshal(proofHeader{
		Typ: proofType,
		Alg: proofAlg,
		JWK: proofJWK{
			Crv: proofCurve,
			Kty: proofKeyType,
			X:   coordinate(key.X.Bytes()),
			Y:   coordinate(key.Y.Bytes()),
		},
	})
	if err != nil {
		return "", fmt.Errorf("deviceclient: encode the proof header: %w", err)
	}
	payload, err := json.Marshal(proofPayload{
		HTM:   method,
		HTP:   path,
		JTI:   base64.RawURLEncoding.EncodeToString(id),
		IatMs: now.UnixMilli(),
	})
	if err != nil {
		return "", fmt.Errorf("deviceclient: encode the proof payload: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) +
		"." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("deviceclient: sign the proof: %w", err)
	}
	// r‖s at fixed width, which is what a JWS ES256 signature is. Never
	// ASN.1: the verifier splits the bytes rather than calling
	// ecdsa.VerifyASN1, whose answer on a DER signature is a silent false.
	signature := make([]byte, 2*coordBytes)
	r.FillBytes(signature[:coordBytes])
	s.FillBytes(signature[coordBytes:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// coordinate renders one affine coordinate at the fixed width RFC 7518
// requires. See the file comment: a short encoding is a different
// thumbprint for the same key.
func coordinate(raw []byte) string {
	fixed := make([]byte, coordBytes)
	// A coordinate longer than the curve's width cannot come from a P-256
	// key, and decodeKey has already refused every other curve; copying
	// the low bytes keeps this total rather than panicking on input that
	// cannot occur.
	if len(raw) > coordBytes {
		raw = raw[len(raw)-coordBytes:]
	}
	copy(fixed[coordBytes-len(raw):], raw)
	return base64.RawURLEncoding.EncodeToString(fixed)
}
