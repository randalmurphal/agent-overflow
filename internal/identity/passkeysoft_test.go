package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// A soft authenticator: an in-process stand-in for a phone, a laptop's
// secure enclave, or a hardware key.
//
// It exists because the alternative is testing the passkey flows against
// captured vectors, which pin what one browser did once and cannot be
// asked what happens when a counter fails to advance, when a person
// declines to verify, or when a credential is presented to the wrong
// relying party. Those are the cases the code makes decisions about, so
// they are the cases the tests have to be able to stage.
//
// It implements exactly what CTAP produces and nothing more: an ES256
// credential, `none` attestation, and the authenticator-data layout of
// §6.1. Every knob on it corresponds to a real authenticator behavior
// this backend has an opinion about.
type softAuthenticator struct {
	key          *ecdsa.PrivateKey
	credentialID []byte
	aaguid       []byte
	signCount    uint32
	// userHandle and rpID are learned from the registration ceremony and
	// replayed on every later assertion, the way an authenticator replays
	// what it stored.
	userHandle []byte
	rpID       string

	// userVerified is the UV flag. A platform authenticator sets it after
	// a fingerprint or a PIN; false is "present but not verified", which
	// is what a step-up must refuse.
	userVerified bool
	// backupEligible and backupState are the synced-credential flags.
	// Eligibility is fixed at registration by a real authenticator, which
	// is why the library refuses an assertion that changes it.
	backupEligible bool
	backupState    bool
	// freezeCounter reproduces the two authenticators this backend must
	// tolerate: the one that keeps no counter at all (always 0), and the
	// cloned-credential case a counter is supposed to reveal. Neither is
	// refused; both are flagged.
	freezeCounter bool
}

func newSoftAuthenticator(t *testing.T) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate authenticator key: %v", err)
	}
	credentialID := make([]byte, 32)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatalf("draw credential id: %v", err)
	}
	return &softAuthenticator{
		key:          key,
		credentialID: credentialID,
		aaguid:       make([]byte, 16),
		userVerified: true,
	}
}

// WebAuthn authenticator-data flag bits (§6.1).
const (
	flagUserPresent    byte = 0x01
	flagUserVerified   byte = 0x04
	flagBackupEligible byte = 0x08
	flagBackupState    byte = 0x10
	flagAttestedData   byte = 0x40
)

// creationOptions is the slice of PublicKeyCredentialCreationOptions an
// authenticator reads.
type creationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// requestOptions is the slice of PublicKeyCredentialRequestOptions an
// authenticator reads.
type requestOptions struct {
	Challenge string `json:"challenge"`
	RPID      string `json:"rpId"`
}

// register answers a creation challenge, recording what the ceremony
// bound the credential to.
func (a *softAuthenticator) register(t *testing.T, challenge PasskeyChallenge, origin string) []byte {
	t.Helper()
	var options creationOptions
	if err := json.Unmarshal(challenge.Options, &options); err != nil {
		t.Fatalf("read creation options: %v", err)
	}
	a.rpID = options.RP.ID
	handle, err := base64.RawURLEncoding.DecodeString(options.User.ID)
	if err != nil {
		t.Fatalf("read user handle: %v", err)
	}
	a.userHandle = handle

	clientData := a.clientData(t, "webauthn.create", options.Challenge, origin)
	authData := a.authenticatorData(true)
	attestation, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		t.Fatalf("encode attestation object: %v", err)
	}
	return mustJSON(t, map[string]any{
		"id":                      b64(a.credentialID),
		"rawId":                   b64(a.credentialID),
		"type":                    "public-key",
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64(clientData),
			"attestationObject": b64(attestation),
			"transports":        []string{"internal", "hybrid"},
		},
	})
}

// assert answers an assertion challenge.
func (a *softAuthenticator) assert(t *testing.T, challenge PasskeyChallenge, origin string) []byte {
	t.Helper()
	var options requestOptions
	if err := json.Unmarshal(challenge.Options, &options); err != nil {
		t.Fatalf("read request options: %v", err)
	}
	return a.assertAgainst(t, options.Challenge, origin)
}

// assertAgainst is assert with the challenge supplied directly, for the
// cases that present an assertion the ceremony did not ask for.
func (a *softAuthenticator) assertAgainst(t *testing.T, challenge, origin string) []byte {
	t.Helper()
	if !a.freezeCounter {
		a.signCount++
	}
	clientData := a.clientData(t, "webauthn.get", challenge, origin)
	authData := a.authenticatorData(false)

	digest := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authData...), digest[:]...)
	signedDigest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, signedDigest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return mustJSON(t, map[string]any{
		"id":                     b64(a.credentialID),
		"rawId":                  b64(a.credentialID),
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64(clientData),
			"authenticatorData": b64(authData),
			"signature":         b64(signature),
			"userHandle":        b64(a.userHandle),
		},
	})
}

func (a *softAuthenticator) clientData(t *testing.T, ceremony, challenge, origin string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"type":        ceremony,
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
}

// authenticatorData renders §6.1's layout: the RP ID hash, the flags, the
// counter, and — for a registration — the attested credential data with
// the COSE public key inside it.
func (a *softAuthenticator) authenticatorData(attested bool) []byte {
	rpIDHash := sha256.Sum256([]byte(a.rpID))
	flags := flagUserPresent
	if a.userVerified {
		flags |= flagUserVerified
	}
	if a.backupEligible {
		flags |= flagBackupEligible
	}
	if a.backupState {
		flags |= flagBackupState
	}
	if attested {
		flags |= flagAttestedData
	}
	out := append([]byte{}, rpIDHash[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.signCount)
	if !attested {
		return out
	}
	out = append(out, a.aaguid...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(a.credentialID)))
	out = append(out, a.credentialID...)
	return append(out, a.coseKey()...)
}

// coseKey encodes the public half the way an authenticator does.
func (a *softAuthenticator) coseKey() []byte {
	key := webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{
			KeyType:   int64(webauthncose.EllipticKey),
			Algorithm: int64(webauthncose.AlgES256),
		},
		Curve:  int64(webauthncose.P256),
		XCoord: a.key.PublicKey.X.FillBytes(make([]byte, 32)),
		YCoord: a.key.PublicKey.Y.FillBytes(make([]byte, 32)),
	}
	encoded, err := webauthncbor.Marshal(key)
	if err != nil {
		panic("identity test: encode COSE key: " + err.Error())
	}
	return encoded
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// testRelyingParty is what every passkey test runs under: "localhost" is
// the one RP ID that is both a valid domain and reachable over plain
// http, which is why it is also the harness and e2e path.
const (
	testRPID    = "localhost"
	testOrigin  = "http://localhost:5173"
	testRPLabel = "Agent Overflow"
)

func testRelyingParty() RelyingParty {
	return RelyingParty{ID: testRPID, DisplayName: testRPLabel, Origins: []string{testOrigin}}
}

// verifyingAssertionOptions asserts the ceremony asked for user
// verification, which is the property a step-up decision rests on.
func assertionDemandsVerification(t *testing.T, challenge PasskeyChallenge) {
	t.Helper()
	var options struct {
		UserVerification string `json:"userVerification"`
	}
	if err := json.Unmarshal(challenge.Options, &options); err != nil {
		t.Fatalf("read request options: %v", err)
	}
	if protocol.UserVerificationRequirement(options.UserVerification) != protocol.VerificationRequired {
		t.Fatalf("the browser was asked for userVerification=%q, want %q",
			options.UserVerification, protocol.VerificationRequired)
	}
}
