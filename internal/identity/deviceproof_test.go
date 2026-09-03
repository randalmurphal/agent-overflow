package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// signingDevice is a test client that holds a real P-256 key and mints
// proofs the way the browser does.
type signingDevice struct {
	key *ecdsa.PrivateKey
	jwk deviceProofKey
}

func newSigningDevice(t *testing.T) *signingDevice {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	return &signingDevice{key: key, jwk: deviceProofKey{
		Crv: deviceProofCurve,
		Kty: deviceProofKeyType,
		X:   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, p256CoordBytes))),
		Y:   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, p256CoordBytes))),
	}}
}

func (d *signingDevice) thumbprint() string { return jwkThumbprint(d.jwk) }

// proof mints one, exactly as a browser would: r‖s, never ASN.1.
func (d *signingDevice) proof(t *testing.T, method, path, jti string, at time.Time) DeviceProof {
	t.Helper()
	return DeviceProof{Value: d.sign(t, deviceProofHeader{
		Typ: deviceProofType, Alg: deviceProofAlg, JWK: d.jwk,
	}, deviceProofPayload{
		HTM: method, HTP: path, JTI: jti, IatMs: at.UnixMilli(),
	}), Method: method, Path: path}
}

func (d *signingDevice) sign(t *testing.T, header deviceProofHeader, payload deviceProofPayload) string {
	t.Helper()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, d.key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 0, deviceProofSigBytes)
	sig = append(sig, r.FillBytes(make([]byte, p256CoordBytes))...)
	sig = append(sig, s.FillBytes(make([]byte, p256CoordBytes))...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// keyPairedDevice walks the real pairing flow for a device that signs: it
// redeems with a proof and the owner confirms. The shape every signed-path
// test starts from.
func keyPairedDevice(t *testing.T, s *Sessions, owner store.User, d *signingDevice, at time.Time) (store.Device, TokenSet) {
	t.Helper()
	link := mustMintLink(t, s, owner)
	redemption, reason := s.RedeemPairing(RedemptionRequest{
		Token: link.Token, Label: "A Phone", Platform: "ios",
		Proof: d.proof(t, "POST", "/auth/pair", "enroll-"+link.Token, at),
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing with a signed proof: %s", reason)
	}
	if _, err := s.ConfirmPairing(redemption.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	device, err := s.store.GetDevice(redemption.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	return device, redemption.Tokens
}

// TestProofVectorFromRealWebCrypto pins the verifier against a proof a
// real Chromium minted, rather than one this package signed for itself.
//
// The reason to freeze bytes rather than round-trip: every other test here
// signs with crypto/ecdsa, so all of them would still pass if Go and
// WebCrypto disagreed about the signature encoding or about which JWK
// members the thumbprint covers. This vector is the only thing in the
// suite that would fail if they did.
//
// Captured by the phase-5 spike (browser: generateKey ECDSA P-256
// non-extractable → exportKey('jwk') → sign). Its three interesting
// properties: the signature is 64 raw bytes rather than ASN.1, the
// exported JWK carries `ext` and `key_ops` members that the RFC 7638
// thumbprint must NOT include, and the resulting thumbprint matches the
// one this package computes.
func TestProofVectorFromRealWebCrypto(t *testing.T) {
	const (
		browserProof = "eyJ0eXAiOiJhby1kZXZpY2UtcHJvb2YrandzIiwiYWxnIjoiRVMyNTYiLCJqd2siOnsiY3J2IjoiUC0yNTYiLCJrdHkiOiJFQyIsIngiOiJqZS1KVVVWSVZ0LVBIMzN5dGRHUVpkMWtaQ1g0NDVfZDExSlF2TUpFR0c0IiwieSI6IkhvaFVOODc0akQySnJXTjJSenRDaHE2ZzhUSm1YcTVoeEp0cTFPcmFaaVEifX0.eyJodG0iOiJQT1NUIiwiaHRwIjoiL2F1dGgvdGlja2V0IiwianRpIjoic3Bpa2UtdmVjdG9yLWp0aS0wMDAxIiwiaWF0TXMiOjE3NjcyMjU2MDAwMDB9.pTA9SrbhOiTgKUtJZQINVXpDSXNSm26QtXViVZnSacSwrIFTNaZhnksX0KHDApgX41Hq60klnWFjI31GmymgKg"
		browserThumb = "EM8N-F-dQ2i2CGdQ8HQeQl5LtG6cJKzvvTsIfmtDw5s"
		browserIatMs = 1767225600000
	)

	parsed, reason := parseDeviceProof(browserProof)
	if reason.Refused() {
		t.Fatalf("a proof minted by a real browser did not parse: %s", reason)
	}
	if parsed.thumbprint != browserThumb {
		t.Fatalf("thumbprint = %q, browser computed %q — the two RFC 7638 "+
			"implementations disagree about which JWK members it covers",
			parsed.thumbprint, browserThumb)
	}
	verified, reason := checkProofSignature(parsed)
	if reason.Refused() {
		t.Fatalf("WebCrypto's signature did not verify: %s "+
			"(WebCrypto emits r‖s, never ASN.1)", reason)
	}
	if reason := verified.boundTo("POST", "/auth/ticket"); reason.Refused() {
		t.Fatalf("binding: %s", reason)
	}
	if reason := verified.withinWindow(browserIatMs); reason.Refused() {
		t.Fatalf("freshness at the minting instant: %s", reason)
	}
}

// TestKeyBoundDeviceRefusesItsOwnThumbprint is the downgrade rule, and the
// point of the whole wave: once a device enrolls a key, the string that
// names that key admits nothing.
func TestKeyBoundDeviceRefusesItsOwnThumbprint(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	device, tokens := keyPairedDevice(t, sessions, owner, signer, c.at)

	if got := ProofKind(device.ProofKind); got != ProofSignedKey {
		t.Fatalf("device enrolled with a signed proof has proof_kind %q, want %q",
			got, ProofSignedKey)
	}
	if device.KeyThumbprint != signer.thumbprint() {
		t.Fatalf("device thumbprint = %q, want the JWK thumbprint %q",
			device.KeyThumbprint, signer.thumbprint())
	}

	session, reason := sessions.Live(tokens.SessionID)
	if reason.Refused() {
		t.Fatalf("Live: %s", reason)
	}
	// The exact presentation every pre-phase-5 client makes.
	if got := sessions.CheckDeviceProof(session, bearerProof(device.KeyThumbprint)); got != ReasonProofDowngraded {
		t.Fatalf("bare thumbprint for a key-bound device = %s, want proof_downgraded", got)
	}
	// And a fresh proof still works, so the refusal is about the SHAPE and
	// not about the device.
	if got := sessions.CheckDeviceProof(session,
		signer.proof(t, "POST", "/auth/ticket", "jti-ok", c.at)); got.Refused() {
		t.Fatalf("a signed proof was refused: %s", got)
	}
}

// TestBearerDeviceKeepsWorkingUnchanged is spec §15 constraint 6: a page
// with no secure context has no key to sign with and must keep the
// behavior it has today, on every route.
func TestBearerDeviceKeepsWorkingUnchanged(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	device, tokens := pairedDevice(t, sessions, owner, "lan-browser-identifier")

	if got := ProofKind(device.ProofKind); got != ProofBearer {
		t.Fatalf("device enrolled with a bare identifier has proof_kind %q, want %q",
			got, ProofBearer)
	}
	session, reason := sessions.Live(tokens.SessionID)
	if reason.Refused() {
		t.Fatalf("Live: %s", reason)
	}
	if got := sessions.CheckDeviceProof(session, bearerProof("lan-browser-identifier")); got.Refused() {
		t.Fatalf("a bearer device was refused its own identifier: %s", got)
	}
	if got := sessions.CheckDeviceProof(session, bearerProof("some-other-identifier")); got != ReasonKeyMismatch {
		t.Fatalf("wrong identifier = %s, want key_mismatch", got)
	}
	// A bearer row's thumbprint is not the hash of any key, so a signature
	// has nothing to be checked against. Refused rather than ignored.
	signer := newSigningDevice(t)
	if got := sessions.CheckDeviceProof(session,
		signer.proof(t, "POST", "/auth/ticket", "jti-1", time.Now())); got != ReasonMalformedProof {
		t.Fatalf("a proof presented for a bearer device = %s, want malformed_proof", got)
	}
}

// TestSignedProofRefusals walks each refusal the signed path can produce.
// Every one of them is a distinct code on purpose: §17 lists replay, skew
// and downgrade as cases needing their own answer, and a shared code would
// make each indistinguishable from a broken signature.
func TestSignedProofRefusals(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	_, tokens := keyPairedDevice(t, sessions, owner, signer, c.at)
	session, reason := sessions.Live(tokens.SessionID)
	if reason.Refused() {
		t.Fatalf("Live: %s", reason)
	}

	t.Run("another key", func(t *testing.T) {
		other := newSigningDevice(t)
		if got := sessions.CheckDeviceProof(session,
			other.proof(t, "POST", "/auth/ticket", "jti-other", c.at)); got != ReasonKeyMismatch {
			t.Fatalf("= %s, want key_mismatch", got)
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		proof := signer.proof(t, "POST", "/auth/ticket", "jti-tampered", c.at)
		// Re-point the payload at another route, keeping the signature. The
		// binding check must never be what catches this — the signature is.
		parts := strings.Split(proof.Value, ".")
		swapped, err := json.Marshal(deviceProofPayload{
			HTM: "POST", HTP: "/auth/token", JTI: "jti-tampered", IatMs: c.at.UnixMilli(),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		proof.Value = parts[0] + "." + base64.RawURLEncoding.EncodeToString(swapped) + "." + parts[2]
		proof.Path = "/auth/token"
		if got := sessions.CheckDeviceProof(session, proof); got != ReasonInvalidSignature {
			t.Fatalf("= %s, want invalid_signature", got)
		}
	})

	t.Run("bound to another request", func(t *testing.T) {
		proof := signer.proof(t, "POST", "/auth/token", "jti-elsewhere", c.at)
		// Presented, intact and correctly signed, on a different route.
		proof.Path = "/auth/ticket"
		if got := sessions.CheckDeviceProof(session, proof); got != ReasonProofNotBound {
			t.Fatalf("= %s, want proof_not_bound", got)
		}
	})

	t.Run("bound to another method", func(t *testing.T) {
		proof := signer.proof(t, "POST", "/auth/ticket", "jti-method", c.at)
		proof.Method = "GET"
		if got := sessions.CheckDeviceProof(session, proof); got != ReasonProofNotBound {
			t.Fatalf("= %s, want proof_not_bound", got)
		}
	})

	t.Run("stale", func(t *testing.T) {
		old := c.at.Add(-deviceProofFreshness - time.Second)
		if got := sessions.CheckDeviceProof(session,
			signer.proof(t, "POST", "/auth/ticket", "jti-stale", old)); got != ReasonOutsideTimeWindow {
			t.Fatalf("= %s, want outside_time_window", got)
		}
	})

	t.Run("future dated", func(t *testing.T) {
		ahead := c.at.Add(deviceProofFreshness + time.Second)
		if got := sessions.CheckDeviceProof(session,
			signer.proof(t, "POST", "/auth/ticket", "jti-ahead", ahead)); got != ReasonOutsideTimeWindow {
			t.Fatalf("= %s, want outside_time_window", got)
		}
	})

	t.Run("replayed", func(t *testing.T) {
		proof := signer.proof(t, "POST", "/auth/ticket", "jti-once", c.at)
		if got := sessions.CheckDeviceProof(session, proof); got.Refused() {
			t.Fatalf("first presentation = %s, want admitted", got)
		}
		if got := sessions.CheckDeviceProof(session, proof); got != ReasonProofReplayed {
			t.Fatalf("second presentation = %s, want proof_replayed", got)
		}
	})

	t.Run("nothing at all", func(t *testing.T) {
		if got := sessions.CheckDeviceProof(session, DeviceProof{}); got != ReasonMissingProof {
			t.Fatalf("= %s, want missing_proof", got)
		}
	})
}

// TestProofParseRefusesEveryMalformedShape covers what the parser must
// reject before anything is trusted. Each case is one thing a proof could
// claim that this build does not accept.
func TestProofParseRefusesEveryMalformedShape(t *testing.T) {
	signer := newSigningDevice(t)
	good := signer.proof(t, "POST", "/auth/ticket", "jti-1", time.Now()).Value
	parts := strings.Split(good, ".")
	seg := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	cases := []struct {
		name  string
		proof string
	}{
		{"empty", ""},
		{"one segment", parts[0]},
		{"two segments", parts[0] + "." + parts[1]},
		{"four segments", good + ".extra"},
		{"empty signature", parts[0] + "." + parts[1] + "."},
		{"not base64url", "!!!." + parts[1] + "." + parts[2]},
		{"header is not JSON", base64.RawURLEncoding.EncodeToString([]byte("nope")) +
			"." + parts[1] + "." + parts[2]},
		// Algorithm agility, refused. `none` is the classic, and any other
		// real algorithm is refused by the same literal comparison.
		{"alg none", seg(map[string]any{"typ": deviceProofType, "alg": "none", "jwk": signer.jwk}) +
			"." + parts[1] + "." + parts[2]},
		{"alg RS256", seg(map[string]any{"typ": deviceProofType, "alg": "RS256", "jwk": signer.jwk}) +
			"." + parts[1] + "." + parts[2]},
		{"another typ", seg(map[string]any{"typ": "dpop+jwt", "alg": deviceProofAlg, "jwk": signer.jwk}) +
			"." + parts[1] + "." + parts[2]},
		{"unknown header member", seg(map[string]any{
			"typ": deviceProofType, "alg": deviceProofAlg, "jwk": signer.jwk, "kid": "x",
		}) + "." + parts[1] + "." + parts[2]},
		{"another curve", seg(map[string]any{"typ": deviceProofType, "alg": deviceProofAlg,
			"jwk": map[string]string{"crv": "P-384", "kty": "EC", "x": signer.jwk.X, "y": signer.jwk.Y},
		}) + "." + parts[1] + "." + parts[2]},
		{"coordinate off the curve", seg(map[string]any{"typ": deviceProofType, "alg": deviceProofAlg,
			"jwk": map[string]string{"crv": deviceProofCurve, "kty": deviceProofKeyType,
				"x": base64.RawURLEncoding.EncodeToString(make([]byte, p256CoordBytes)),
				"y": base64.RawURLEncoding.EncodeToString(make([]byte, p256CoordBytes))},
		}) + "." + parts[1] + "." + parts[2]},
		{"short coordinate", seg(map[string]any{"typ": deviceProofType, "alg": deviceProofAlg,
			"jwk": map[string]string{"crv": deviceProofCurve, "kty": deviceProofKeyType,
				"x": base64.RawURLEncoding.EncodeToString(make([]byte, p256CoordBytes-1)),
				"y": signer.jwk.Y},
		}) + "." + parts[1] + "." + parts[2]},
		{"no jti", parts[0] + "." + seg(map[string]any{
			"htm": "POST", "htp": "/auth/ticket", "iatMs": time.Now().UnixMilli(),
		}) + "." + parts[2]},
		{"unknown payload member", parts[0] + "." + seg(map[string]any{
			"htm": "POST", "htp": "/auth/ticket", "jti": "j", "iatMs": time.Now().UnixMilli(),
			"ath": "x",
		}) + "." + parts[2]},
		{"signature is not 64 bytes", parts[0] + "." + parts[1] + "." +
			base64.RawURLEncoding.EncodeToString(make([]byte, 63))},
		{"oversized", parts[0] + "." + parts[1] + "." +
			base64.RawURLEncoding.EncodeToString(make([]byte, maxDeviceProofBytes))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, reason := parseDeviceProof(tc.proof); reason != ReasonMalformedProof {
				t.Fatalf("= %s, want malformed_proof", reason)
			}
		})
	}
}

// TestOversizedJTIIsRefused keeps the replay guard's memory a function of
// the request rate rather than of what a client puts in one field.
func TestOversizedJTIIsRefused(t *testing.T) {
	signer := newSigningDevice(t)
	proof := signer.proof(t, "POST", "/auth/ticket",
		strings.Repeat("j", maxDeviceProofJTIBytes+1), time.Now())
	if _, reason := parseDeviceProof(proof.Value); reason != ReasonMalformedProof {
		t.Fatalf("= %s, want malformed_proof", reason)
	}
}

// TestEnrollmentRefusesAnUnverifiableProofWithoutSpendingTheLink: a
// client-side signing bug must not cost the link somebody minted, because
// a spent link and a broken client would then be indistinguishable to the
// person holding the phone.
func TestEnrollmentRefusesAnUnverifiableProofWithoutSpendingTheLink(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	link := mustMintLink(t, sessions, owner)

	// A proof minted for another route: verifies, but is not bound to this
	// redemption.
	bad := signer.proof(t, "POST", "/auth/token", "jti-wrong-route", c.at)
	bad.Path = "/auth/pair"
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bad,
	}); reason != ReasonProofNotBound {
		t.Fatalf("= %s, want proof_not_bound", reason)
	}

	// The link survived, so the same device can correct itself.
	redemption, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token,
		Proof: signer.proof(t, "POST", "/auth/pair", "jti-retry", c.at),
	})
	if reason.Refused() {
		t.Fatalf("retry after a refused proof: %s", reason)
	}
	if redemption.DeviceID == "" {
		t.Fatal("retry produced no device")
	}
}

// TestRepairingCannotDowngradeAKeyBoundDevice closes the one path that
// writes device rows: adopting an existing row must never change what its
// thumbprint MEANS, or the row's own key requirement could be undone by
// redeeming a fresh link with the thumbprint as a bare string.
func TestRepairingCannotDowngradeAKeyBoundDevice(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	device, _ := keyPairedDevice(t, sessions, owner, signer, c.at)

	link := mustMintLink(t, sessions, owner)
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof(device.KeyThumbprint),
	}); reason != ReasonProofDowngraded {
		t.Fatalf("re-pairing a key-bound device as bearer = %s, want proof_downgraded", reason)
	}

	// The row is untouched: still key-bound, still the same device.
	after, err := sessions.store.GetDevice(device.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if ProofKind(after.ProofKind) != ProofSignedKey {
		t.Fatalf("proof_kind became %q", after.ProofKind)
	}
}

// TestRepairingAdoptsTheSameKeyBoundRow is the other half: a device that
// re-pairs with the SAME key is the same physical device and must not
// accumulate rows (the property the unique thumbprint index exists for).
func TestRepairingAdoptsTheSameKeyBoundRow(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	first, _ := keyPairedDevice(t, sessions, owner, signer, c.at)
	second, _ := keyPairedDevice(t, sessions, owner, signer, c.at.Add(time.Minute))
	if first.ID != second.ID {
		t.Fatalf("re-pairing one key produced two device rows: %s and %s", first.ID, second.ID)
	}
}

// TestRefreshBindsToTheSignedProof: rotation is the path the spec singles
// out ("refresh binds to the device key on every listener"), so it gets
// the proof rule explicitly rather than by inheritance.
func TestRefreshBindsToTheSignedProof(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	signer := newSigningDevice(t)
	device, tokens := keyPairedDevice(t, sessions, owner, signer, c.at)

	// The bare thumbprint is refused, and — the reason the ordering inside
	// Refresh is what it is — the secret is NOT spent by the refusal.
	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret, Proof: bearerProof(device.KeyThumbprint),
	}); reason != ReasonProofDowngraded {
		t.Fatalf("renewal on a bare thumbprint = %s, want proof_downgraded", reason)
	}
	renewed, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret,
		Proof:  signer.proof(t, "POST", "/auth/token", "jti-renew", c.at),
	})
	if reason.Refused() {
		t.Fatalf("renewal with a signed proof after a refused one: %s", reason)
	}
	if renewed.SessionID != tokens.SessionID {
		t.Fatal("renewal moved the session id")
	}
}
