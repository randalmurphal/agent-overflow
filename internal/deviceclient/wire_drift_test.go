package deviceclient

import (
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transport"
)

// This package restates the wire it speaks — the route paths, the header
// names, the ticket parameter, and the pairing payload's shape — because
// it deliberately imports neither `internal/transport` nor
// `internal/identity` in production code. The precedent is
// `internal/relaysession`, which restates the transport's cookie prefix
// and session header for the same reason and pins them the same way.
//
// So the restatement is only safe while something fails when it drifts.
// That is this file, and only this file: the imports above exist in a
// TEST and nowhere else.

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

// TestWireConstantsMatchTheTransport pins every spelling this package
// restates against the package that defines it. A rename on either side is
// a failing test rather than a client that dials a route the backend does
// not serve.
func TestWireConstantsMatchTheTransport(t *testing.T) {
	for name, pair := range map[string][2]string{
		"pair route":        {authPairPath, transport.AuthPairPath},
		"token route":       {authTokenPath, transport.AuthTokenPath},
		"ticket route":      {authTicketPath, transport.AuthTicketPath},
		"bootstrap route":   {bootstrapPath, transport.BootstrapPath},
		"upgrade route":     {wsPath, transport.WSPath},
		"session header":    {SessionCredentialHeader, transport.SessionCredentialHeader},
		"device key header": {DeviceKeyHeader, transport.DeviceKeyHeader},
		"ticket parameter":  {WSTicketParam, transport.WSTicketParam},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: this package says %q, the transport says %q", name, pair[0], pair[1])
		}
	}
	if LinkVersion != identity.PairingPayloadVersion {
		t.Errorf("link version = %d, identity mints %d", LinkVersion, identity.PairingPayloadVersion)
	}
	if reasonPendingConfirmation != identity.ReasonPendingConfirmation.Code() {
		t.Errorf("pending reason = %q, identity spells it %q",
			reasonPendingConfirmation, identity.ReasonPendingConfirmation.Code())
	}
}

// TestDecodeLinkReadsAPayloadIdentityActuallyMinted is the round trip that
// catches a field added on the minting side without a field here, which
// would otherwise decode to its zero value in silence. CertFingerprint is
// the one that matters most: absent, this client dials unpinned.
func TestDecodeLinkReadsAPayloadIdentityActuallyMinted(t *testing.T) {
	minted := identity.PairingPayload{
		Version:         identity.PairingPayloadVersion,
		BackendID:       "backend-0001",
		BackendName:     "Studio",
		Endpoint:        "http://192.168.1.5:8317",
		Token:           "link-token",
		CertFingerprint: "sha256:0badc0de",
	}
	encoded, err := minted.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := DecodeLink("http://192.168.1.5:8317/#" + LinkFragmentPrefix + encoded)
	if err != nil {
		t.Fatalf("DecodeLink: %v", err)
	}
	want := Link{
		Version:         minted.Version,
		BackendID:       minted.BackendID,
		BackendName:     minted.BackendName,
		Endpoint:        minted.Endpoint,
		Token:           minted.Token,
		CertFingerprint: minted.CertFingerprint,
	}
	if got != want {
		t.Fatalf("DecodeLink = %+v, want %+v", got, want)
	}
}

// TestMintedProofVerifiesAgainstIdentitysOwnVerifier is the one test that
// would fail if this client and the backend disagreed about the proof
// shape — every other test here signs and checks nothing.
//
// It goes through a real redemption rather than a verifier call, because
// that is the path where the three easy mistakes actually bite: `htp` as a
// full URI rather than a path, `iatMs` in seconds rather than
// milliseconds, and a coordinate rendered at its minimal width, which
// hashes to a thumbprint the backend then cannot match.
func TestMintedProofVerifiesAgainstIdentitysOwnVerifier(t *testing.T) {
	st := storetest.Clone(t)
	sessions, boot, err := identity.Bootstrap(st, "backend-0001", "Owner")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	link, err := sessions.MintPairingLink(identity.PairingRequest{
		UserID:       boot.Owner.ID,
		DeviceClass:  identity.DeviceDesktop,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       identity.Scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}

	key, err := EnrollDeviceKey(t.TempDir())
	if err != nil {
		t.Fatalf("EnrollDeviceKey: %v", err)
	}
	proof, err := mintProof(key, "POST", authPairPath, time.Now())
	if err != nil {
		t.Fatalf("mintProof: %v", err)
	}

	redemption, reason := sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token,
		Proof: identity.DeviceProof{Value: proof, Method: "POST", Path: authPairPath},
		Label: "drift guard",
		Peer:  "127.0.0.1:1",
	})
	if reason.Refused() {
		t.Fatalf("the backend refused a proof this client minted: %s", reason.Code())
	}
	if redemption.VerificationNumber == "" {
		t.Fatal("the redemption carried no verification number")
	}

	// The proof enrolled a KEY device, not a bearer one. A client that
	// rendered its JWK differently could still redeem — as a weaker row —
	// so the class is what proves the signature was read.
	devices, err := st.ListDevicesForUser(boot.Owner.ID)
	if err != nil {
		t.Fatalf("ListDevicesForUser: %v", err)
	}
	var device store.Device
	for _, candidate := range devices {
		if candidate.ID == redemption.DeviceID {
			device = candidate
		}
	}
	if device.ID == "" {
		t.Fatalf("the redemption named device %q, which the owner does not hold", redemption.DeviceID)
	}
	if device.ProofKind != string(identity.ProofSignedKey) {
		t.Fatalf("device enrolled as %q, want a key-bound row", device.ProofKind)
	}

	// And the number the owner's screen shows is derived from the key this
	// client actually presented, which is what makes a mismatch mean "some
	// other device redeemed this link".
	owner, err := sessions.VerificationNumber(link.Link.ID, device.KeyThumbprint)
	if err != nil {
		t.Fatalf("VerificationNumber: %v", err)
	}
	if owner != redemption.VerificationNumber {
		t.Fatalf("the two screens show %q and %q", owner, redemption.VerificationNumber)
	}
	// Six digits, leading zeros kept: a five-digit number on one screen
	// and six on the other is a confirmation nobody completes.
	if len(owner) != 6 || strings.Trim(owner, "0123456789") != "" {
		t.Fatalf("verification number %q is not six digits", owner)
	}
}
