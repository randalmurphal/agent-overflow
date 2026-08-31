package identity

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestClaimsRoundTrip(t *testing.T) {
	secret := []byte("a 32 byte secret for the mac....")
	want := Claims{
		KeyID:     "0123456789abcdef",
		SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt:  1_700_000_000_000,
		ExpiresAt: 1_700_000_060_000,
	}
	credential, err := signClaims(want, secret, testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if !strings.HasPrefix(credential, claimsPrefix) {
		t.Fatalf("credential %q does not carry the version prefix", credential)
	}
	got, payload, mac, reason := parseClaims(credential)
	if reason.Refused() {
		t.Fatalf("parseClaims refused a credential it just produced: %s", reason)
	}
	if got != want {
		t.Fatalf("claims did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	computed := claimsMAC(payload[:], secret, testBackendID)
	if computed != mac {
		t.Fatal("recomputed mac does not match the one carried")
	}
}

// TestCredentialIsFixedLength — the encoding is a fixed layout, so its
// wire size is a fact, not a range. A change to the layout has to move
// this number deliberately.
func TestCredentialIsFixedLength(t *testing.T) {
	credential, err := signClaims(Claims{
		KeyID:     "0123456789abcdef",
		SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt:  1, ExpiresAt: 2,
	}, []byte("secret"), testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if len(credential) != 103 {
		t.Fatalf("credential is %d bytes, want 103", len(credential))
	}
}

func TestParseClaimsRefusesEveryMalformedShape(t *testing.T) {
	valid, err := signClaims(Claims{
		KeyID:     "0123456789abcdef",
		SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt:  1, ExpiresAt: 2,
	}, []byte("secret"), testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}

	cases := map[string]string{
		"empty":               "",
		"prefix only":         claimsPrefix,
		"wrong prefix":        "ao2." + valid[len(claimsPrefix):],
		"truncated":           valid[:len(valid)-1],
		"extended":            valid + "A",
		"separator moved":     strings.Replace(valid, ".", "-", 2),
		"payload not base64":  valid[:4] + "!!" + valid[6:],
		"mac not base64":      valid[:len(valid)-2] + "!!",
		"no separators":       strings.ReplaceAll(valid, ".", "A"),
		"unknown version":     mutateVersionByte(t, valid),
		"whitespace prefixed": " " + valid[1:],
	}
	for name, credential := range cases {
		_, _, _, reason := parseClaims(credential)
		if reason != ReasonMalformedProof {
			t.Fatalf("%s: parseClaims = %s, want malformed_proof", name, reason)
		}
	}
}

// mutateVersionByte re-signs a payload whose leading version byte is not
// the one this build speaks, so the version check is exercised through a
// structurally valid credential rather than random noise.
func mutateVersionByte(t *testing.T, credential string) string {
	t.Helper()
	_, payload, _, reason := parseClaims(credential)
	if reason.Refused() {
		t.Fatalf("fixture credential did not parse: %s", reason)
	}
	payload[0] = claimsVersion + 1
	return claimsPrefix + base64.RawURLEncoding.EncodeToString(payload[:]) +
		"." + strings.Split(credential, ".")[2]
}

func TestClaimsRefuseUnparseableInputAtSignTime(t *testing.T) {
	secret := []byte("secret")
	if _, err := signClaims(Claims{KeyID: "short", SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}, secret, testBackendID); err == nil {
		t.Fatal("signClaims accepted a key id that is not 16 hex characters")
	}
	if _, err := signClaims(Claims{KeyID: "0123456789abcdeg", SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}, secret, testBackendID); err == nil {
		t.Fatal("signClaims accepted a key id that is not hex")
	}
	if _, err := signClaims(Claims{KeyID: "0123456789abcdef", SessionID: "not-a-uuid"}, secret, testBackendID); err == nil {
		t.Fatal("signClaims accepted a session id that is not a uuid")
	}
}

// TestMACBindsToTheBackendIdentity — a database restored under a new
// backend id must refuse every session it imported, which is the
// re-pairing recovery the spec already states rather than a second
// mechanism bolted on.
func TestMACBindsToTheBackendIdentity(t *testing.T) {
	secret := []byte("secret")
	claims := Claims{
		KeyID:     "0123456789abcdef",
		SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt:  1, ExpiresAt: 2,
	}
	credential, err := signClaims(claims, secret, "backend-a")
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	_, payload, mac, reason := parseClaims(credential)
	if reason.Refused() {
		t.Fatalf("parseClaims: %s", reason)
	}
	if claimsMAC(payload[:], secret, "backend-b") == mac {
		t.Fatal("the same payload and secret verified under a different backend id")
	}
	if claimsMAC(payload[:], []byte("other secret"), "backend-a") == mac {
		t.Fatal("a different secret produced the same mac")
	}
}

func TestKeyIDEncodingRoundTrips(t *testing.T) {
	raw := []byte{0x00, 0x0f, 0x10, 0x7f, 0x80, 0xa5, 0xfe, 0xff}
	encoded := encodeKeyID(raw)
	if encoded != "000f107f80a5feff" {
		t.Fatalf("encodeKeyID = %q", encoded)
	}
	decoded, err := decodeKeyID(encoded)
	if err != nil {
		t.Fatalf("decodeKeyID: %v", err)
	}
	if string(decoded[:]) != string(raw) {
		t.Fatalf("key id did not round-trip: %x", decoded)
	}
	// Uppercase hex is a different id, not the same one: the store's
	// primary key is the exact string, so accepting both spellings would
	// make one key look like two.
	if _, err := decodeKeyID("000F107F80A5FEFF"); err == nil {
		t.Fatal("decodeKeyID accepted uppercase hex")
	}
}

// TestWithinWindowSeparatesSkewFromExpiry — the two failures have opposite
// remedies (fix a clock vs renew a credential), so they are different
// reasons even though both are "the timestamps say no".
func TestWithinWindowSeparatesSkewFromExpiry(t *testing.T) {
	const now = 1_000_000
	verified := verifiedClaims{claims: Claims{IssuedAt: now, ExpiresAt: now + 1000}}
	if reason := verified.withinWindow(now); reason.Refused() {
		t.Fatalf("a current credential was refused: %s", reason)
	}

	future := verifiedClaims{claims: Claims{
		IssuedAt: now + maxFutureSkewMillis + 1, ExpiresAt: now + maxFutureSkewMillis + 1000,
	}}
	if reason := future.withinWindow(now); reason != ReasonOutsideTimeWindow {
		t.Fatalf("future-dated credential = %s, want outside_time_window", reason)
	}

	// Inside the tolerance is admitted: an NTP correction landing after
	// boot must not lock someone out.
	tolerated := verifiedClaims{claims: Claims{
		IssuedAt: now + maxFutureSkewMillis, ExpiresAt: now + maxFutureSkewMillis + 1000,
	}}
	if reason := tolerated.withinWindow(now); reason.Refused() {
		t.Fatalf("a credential inside the skew tolerance was refused: %s", reason)
	}

	expired := verifiedClaims{claims: Claims{IssuedAt: now - 1000, ExpiresAt: now}}
	if reason := expired.withinWindow(now); reason != ReasonExpiredSession {
		t.Fatalf("credential at its own expiry = %s, want expired_session", reason)
	}
}
