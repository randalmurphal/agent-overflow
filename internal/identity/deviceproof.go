package identity

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"time"
)

// The signed device-key proof (docs/specs/remote-access.md §4, phase 5).
//
// What it replaces. Until this file existed, a device proved possession of
// its key by presenting the key's THUMBPRINT — a string. A string copied
// out of a page's storage is as good as the key it names, so the binding
// bought attribution and nothing more. A proof is a fresh signature over
// the request being made, so presenting a copy of a credential is no
// longer enough on any path that binds to a device key.
//
// Shape. A compact JWS, `base64url(header).base64url(payload).base64url(sig)`,
// carried on transport.DeviceKeyHeader — the same header the thumbprint
// rode, which is why every call site was already written for this (see
// that constant's doc comment).
//
//	header  {"typ":"ao-device-proof+jws","alg":"ES256",
//	         "jwk":{"crv":"P-256","kty":"EC","x":"…","y":"…"}}
//	payload {"htm":"POST","htp":"/auth/token","jti":"…","iatMs":1234567890123}
//
// Adapted from RFC 9449 (OAuth DPoP), with its two useful properties kept
// intact: the public key travels inside the proof so the server needs no
// key registry beyond the thumbprint it already stores, and the payload
// binds the proof to one call so a proof captured on one route cannot be
// presented on another.
//
// Two deliberate departures from RFC 9449, which is why the field names
// and `typ` are ours rather than the RFC's — a reader who knows DPoP must
// not read these as the RFC's fields and be wrong:
//
//   - `htp` carries the request PATH, where the RFC's `htu` carries the
//     full target URI. One backend answers on several authorities at once
//     — loopback, a LAN address, the WSL launcher's relay port, a
//     `--connect` stub's reverse proxy — and every one of them is the same
//     backend reached through a different hop. Binding the authority would
//     refuse a proof for arriving through a hop the client cannot predict,
//     while the path is what actually distinguishes /auth/token from
//     /auth/ticket, which is the binding worth having.
//   - `iatMs` is Unix MILLISECONDS, where JWT `iat` is seconds. Every
//     timestamp on this wire is milliseconds; reusing the RFC's spelling
//     for a different unit is how a factor-of-1000 bug ships.
//
// Cost. Verification is one SHA-256 and one P-256 verify, on the order of
// tens of microseconds, and it runs per HTTP request and per WS upgrade —
// never per frame. That bound is spec §14's, and it is the reason the
// per-RPC path (Sessions.Live) does no signature work at all.

const (
	// deviceProofType is the exact `typ` a proof must carry. Pinned to one
	// literal: it is what stops a JWS minted for something else — now or
	// by a later feature on the same key — from being presentable here.
	deviceProofType = "ao-device-proof+jws"

	// deviceProofAlg is the only signature algorithm. ES256, and nothing
	// else, ever.
	//
	// No algorithm agility, for the reason claims.go carries no `alg`
	// field at all: a presentation that can propose its own algorithm can
	// propose the weakest one this build still understands. There is no
	// migration story that needs two, because a device that must change
	// algorithm re-pairs, which it can already do.
	deviceProofAlg = "ES256"

	// deviceProofCurve and deviceProofKeyType are the only key this
	// verifier accepts, matching WebCrypto's `ECDSA` / `P-256`.
	deviceProofCurve   = "P-256"
	deviceProofKeyType = "EC"

	// p256CoordBytes is the fixed width of a P-256 affine coordinate. A
	// JWK whose x or y decodes to any other length is malformed, not a
	// number to left-pad: RFC 7518 requires the fixed width, and accepting
	// a short encoding would make two spellings of one key hash to two
	// thumbprints.
	p256CoordBytes = 32

	// deviceProofSigBytes is the length of an ES256 signature: r‖s, each
	// a fixed-width P-256 scalar. WebCrypto emits exactly this (never
	// ASN.1), which the phase-5 spike confirmed against a real Chromium —
	// and which is why verification below splits the bytes rather than
	// calling ecdsa.VerifyASN1, whose answer on these bytes is a silent
	// false.
	deviceProofSigBytes = 2 * p256CoordBytes

	// maxDeviceProofBytes bounds one proof. A real proof is ~400 bytes;
	// the cap is generous against that and small enough that a wedged
	// client cannot make the backend parse on its behalf.
	maxDeviceProofBytes = 4 << 10

	// maxDeviceProofJTIBytes bounds the replay identifier, because it is
	// the one field a client chooses freely AND the backend retains. 128
	// characters is far past the 128 bits of randomness a client needs;
	// the cap is what keeps the replay guard's memory a function of the
	// request RATE rather than of what a client puts in one field.
	maxDeviceProofJTIBytes = 128

	// deviceProofFreshness is how far a proof's `iatMs` may sit from this
	// host's clock, in EITHER direction.
	//
	// Symmetric, unlike the session claims' one-sided skew, because the
	// two answer different questions. A credential this backend signed
	// carries its own expiry, so only a future date is suspicious; a proof
	// carries no expiry at all, and this window IS its lifetime — a proof
	// captured an hour ago must not still be presentable.
	//
	// Two minutes matches maxFutureSkewMillis and covers the same real
	// causes: an NTP correction landing after boot, a resumed VM, a phone
	// that has not synced. Beyond it the clocks genuinely disagree, and
	// ReasonOutsideTimeWindow's hint says so.
	deviceProofFreshness = 2 * time.Minute
)

// DeviceProof is one presentation of a device's proof-of-possession, as
// the request carrying it presented it.
//
// A value type with no request in it: this package never sees an
// *http.Request (see the package doc's layering note), so the caller that
// does — internal/app's SessionForRequest, which is the one hook every
// presentation path runs through — reads the three fields off the request
// and passes them in. Method and Path are ignored for a `bearer` device
// and load-bearing for a `key` one.
type DeviceProof struct {
	// Value is the transport.DeviceKeyHeader value: a compact JWS for a
	// key-bound device, or the bare enrollment thumbprint for a bearer
	// one. Which of the two is ACCEPTED is decided by the device row, not
	// by what arrived — that is the downgrade rule.
	Value string
	// Method is the request's HTTP method, which a signed proof binds.
	Method string
	// Path is the request's URL path, which a signed proof binds. The path
	// alone, never the authority — see the file comment.
	Path string
}

// Signed reports whether Value is shaped like a compact JWS rather than a
// bare thumbprint.
//
// Two dots is the whole test, and it is enough because the two shapes do
// not overlap: an enrollment thumbprint is base64url, whose alphabet
// contains no '.' at all.
//
// This decides which shape was PRESENTED. It never decides which shape is
// accepted — a device row's ProofKind decides that, and it is why a
// key-bound device presenting a bearer value is refused instead of quietly
// taking the weaker path.
func (p DeviceProof) Signed() bool {
	return strings.Count(p.Value, ".") == 2
}

// deviceProofHeader is the JWS protected header.
type deviceProofHeader struct {
	Typ string         `json:"typ"`
	Alg string         `json:"alg"`
	JWK deviceProofKey `json:"jwk"`
}

// deviceProofKey is the embedded public key: an EC JWK with exactly the
// four members RFC 7638 requires for the kind, and no others.
//
// Only these four are decoded, which is also what makes the thumbprint
// well-defined — RFC 7638 hashes the required members alone, so `ext` and
// `key_ops` (which WebCrypto's exportKey does emit) must not reach it.
type deviceProofKey struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// deviceProofPayload is what the signature covers.
type deviceProofPayload struct {
	// HTM is the HTTP method this proof was minted for.
	HTM string `json:"htm"`
	// HTP is the request path this proof was minted for.
	HTP string `json:"htp"`
	// JTI is the proof's unique identifier, spent once by the replay
	// guard.
	JTI string `json:"jti"`
	// IatMs is when the client minted it, in Unix milliseconds.
	IatMs int64 `json:"iatMs"`
}

// parsedDeviceProof is a proof whose structure was read and whose
// signature has NOT been checked. Every field is caller-supplied until it
// has, which is why nothing outside this file can hold one.
type parsedDeviceProof struct {
	payload deviceProofPayload
	// thumbprint is the RFC 7638 thumbprint of the embedded key. Computed
	// during parsing because the comparison against the device row is what
	// decides WHICH key we are about to verify under.
	thumbprint string
	pub        *ecdsa.PublicKey
	// signingInput is `header.payload` exactly as it arrived — the bytes
	// the client signed, never a re-serialization of the decoded structs.
	// Re-encoding would make verification depend on this build's JSON
	// field order.
	signingInput []byte
	signature    []byte
}

// verifiedDeviceProof is a parsedDeviceProof whose signature verified.
//
// The same ordering guarantee verifiedClaims provides, for the same
// refusal: the freshness window is reachable only through withinWindow,
// which is a method on THIS type, and the only constructor is
// checkProofSignature's success path. So no arrangement of calls — none a
// later edit can introduce either — reports a proof that did not verify as
// a clock problem, and ReasonOutsideTimeWindow's "check automatic date &
// time" hint therefore never appears for a proof this backend's device
// never signed.
//
// The request binding rides the same type for the same reason: telling a
// client "you signed the wrong path" about a signature we never checked
// would be a diagnosis of a document nobody wrote.
type verifiedDeviceProof struct {
	proof parsedDeviceProof
}

// parseDeviceProof reads a compact JWS's structure and recomputes the
// thumbprint of the key inside it. It learns nothing about authenticity.
func parseDeviceProof(raw string) (parsedDeviceProof, Reason) {
	if len(raw) > maxDeviceProofBytes {
		return parsedDeviceProof{}, ReasonMalformedProof
	}
	firstDot := strings.IndexByte(raw, '.')
	lastDot := strings.LastIndexByte(raw, '.')
	if firstDot <= 0 || lastDot <= firstDot+1 || lastDot == len(raw)-1 {
		return parsedDeviceProof{}, ReasonMalformedProof
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(raw[:firstDot])
	if err != nil {
		return parsedDeviceProof{}, ReasonMalformedProof
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(raw[firstDot+1 : lastDot])
	if err != nil {
		return parsedDeviceProof{}, ReasonMalformedProof
	}
	signature, err := base64.RawURLEncoding.DecodeString(raw[lastDot+1:])
	if err != nil || len(signature) != deviceProofSigBytes {
		return parsedDeviceProof{}, ReasonMalformedProof
	}

	var header deviceProofHeader
	if err := strictJSON(headerBytes, &header); err != nil {
		return parsedDeviceProof{}, ReasonMalformedProof
	}
	// Pinned before anything is done with the key. A header naming another
	// algorithm is refused here rather than handled, which is what "no
	// algorithm agility" means as code.
	if header.Typ != deviceProofType || header.Alg != deviceProofAlg {
		return parsedDeviceProof{}, ReasonMalformedProof
	}

	pub, reason := decodeProofKey(header.JWK)
	if reason.Refused() {
		return parsedDeviceProof{}, reason
	}

	var payload deviceProofPayload
	if err := strictJSON(payloadBytes, &payload); err != nil {
		return parsedDeviceProof{}, ReasonMalformedProof
	}
	if payload.JTI == "" || len(payload.JTI) > maxDeviceProofJTIBytes ||
		payload.HTM == "" || payload.HTP == "" || payload.IatMs <= 0 {
		return parsedDeviceProof{}, ReasonMalformedProof
	}

	return parsedDeviceProof{
		payload:      payload,
		thumbprint:   jwkThumbprint(header.JWK),
		pub:          pub,
		signingInput: []byte(raw[:lastDot]),
		signature:    signature,
	}, ReasonNone
}

// decodeProofKey turns the embedded JWK into a usable public key, refusing
// anything that is not a point on P-256.
//
// The on-curve check is `ecdh.P256().NewPublicKey`, not the deprecated
// elliptic.Unmarshal, which does not validate. ecdsa.Verify would refuse
// an off-curve point too, but as an invalid signature — and "this is not a
// key" is a different fact from "this signature is wrong", so it is said
// where it is true.
func decodeProofKey(jwk deviceProofKey) (*ecdsa.PublicKey, Reason) {
	if jwk.Kty != deviceProofKeyType || jwk.Crv != deviceProofCurve {
		return nil, ReasonMalformedProof
	}
	x, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil || len(x) != p256CoordBytes {
		return nil, ReasonMalformedProof
	}
	y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil || len(y) != p256CoordBytes {
		return nil, ReasonMalformedProof
	}
	point := make([]byte, 0, 1+2*p256CoordBytes)
	point = append(point, 0x04) // uncompressed
	point = append(point, x...)
	point = append(point, y...)
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return nil, ReasonMalformedProof
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, ReasonNone
}

// jwkThumbprint is RFC 7638 over an EC JWK: SHA-256 of the required
// members, lexicographically ordered, no whitespace, base64url unpadded.
//
// Built with a fixed format string rather than json.Marshal because RFC
// 7638 specifies the exact bytes and a struct's field order is this
// build's business, not the specification's. The interpolated values are
// safe to place unquoted-by-the-encoder because callers reach this only
// after decodeProofKey has proved `crv` and `kty` equal to their literals
// and `x` and `y` to be valid base64url of a fixed length — an alphabet
// with no quote, backslash, or control character in it.
//
// The phase-5 spike pinned this against a real browser: the same key
// hashed in Chromium's WebCrypto and here produced identical thumbprints,
// and TestProofVectorFromRealWebCrypto keeps it that way.
func jwkThumbprint(jwk deviceProofKey) string {
	sum := sha256.Sum256([]byte(
		`{"crv":"` + jwk.Crv + `","kty":"` + jwk.Kty + `","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// checkProofSignature verifies the signature and is the ONLY constructor
// of verifiedDeviceProof. See that type for why that matters.
func checkProofSignature(proof parsedDeviceProof) (verifiedDeviceProof, Reason) {
	digest := sha256.Sum256(proof.signingInput)
	r := new(big.Int).SetBytes(proof.signature[:p256CoordBytes])
	s := new(big.Int).SetBytes(proof.signature[p256CoordBytes:])
	if !ecdsa.Verify(proof.pub, digest[:], r, s) {
		return verifiedDeviceProof{}, ReasonInvalidSignature
	}
	return verifiedDeviceProof{proof: proof}, ReasonNone
}

// boundTo checks that this proof was minted for the request carrying it.
//
// Exact comparison on both. The path is compared as the server routed it,
// which is the same string the client fetched, because these are
// same-origin fetches against literal paths — there is no normalisation
// step whose two implementations could disagree.
func (v verifiedDeviceProof) boundTo(method, path string) Reason {
	if v.proof.payload.HTM != method || v.proof.payload.HTP != path {
		return ReasonProofNotBound
	}
	return ReasonNone
}

// withinWindow checks the proof's freshness against now (Unix
// milliseconds), in both directions. See deviceProofFreshness for why the
// window is symmetric and why it is the proof's whole lifetime.
func (v verifiedDeviceProof) withinWindow(now int64) Reason {
	skew := v.proof.payload.IatMs - now
	if skew < 0 {
		skew = -skew
	}
	if skew > deviceProofFreshness.Milliseconds() {
		return ReasonOutsideTimeWindow
	}
	return ReasonNone
}

// admitProof runs the half of a proof presentation that follows parsing:
// signature, request binding, freshness, replay — in that order.
//
// One function because there are two callers and their orderings must not
// be able to drift: a request presented against an enrolled device row
// (Sessions.verifyDeviceProof) and a redemption that has no row yet
// (Sessions.enrollmentFor). What differs between them is only whether a
// stored thumbprint exists to compare the embedded key against, and that
// check sits OUTSIDE this function on both sides for exactly that reason.
//
// Each position is argued at Sessions.verifyDeviceProof, which is the call
// site a reader arrives at first.
func (s *Sessions) admitProof(parsed parsedDeviceProof, presented DeviceProof) Reason {
	verified, reason := checkProofSignature(parsed)
	if reason.Refused() {
		return reason
	}
	if reason := verified.boundTo(presented.Method, presented.Path); reason.Refused() {
		return reason
	}
	now := s.now().UnixMilli()
	if reason := verified.withinWindow(now); reason.Refused() {
		return reason
	}
	if !s.proofs.admit(parsed.payload.JTI, now) {
		return ReasonProofReplayed
	}
	return ReasonNone
}

// strictJSON decodes exactly one JSON object and refuses unknown members.
//
// Strict on both counts. Anything but a clean EOF after the first document
// is the shape a smuggling relay produces, and an unknown member in a
// signed structure is a field this build does not understand inside
// something it is about to act on — for a proof, that is a claim being
// made that nothing here checks. The client and the server are the same
// project, so there is no forward-compatibility cost: a new member ships
// with the code that reads it.
func strictJSON(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errTrailingProofJSON
	}
	return nil
}

// errTrailingProofJSON is what strictJSON answers for anything following
// the first document.
var errTrailingProofJSON = errors.New("identity: trailing bytes after the JSON document")
