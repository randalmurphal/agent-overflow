package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"

	"github.com/coder/websocket"
)

// This file is the cross-host half of wave 8c, proved end to end against
// the real stack: a real transport listener terminating real TLS with a
// real `internal/servercert` certificate, a real `internal/identity`
// session core behind it, and a real `internal/deviceclient` on the other
// side of the network.
//
// It lives here because `internal/deviceclient` deliberately imports
// neither of the two packages it speaks to, so this is the only place in
// the tree that can hold both ends of one exchange.

// pairedBackend is a booted backend a device can pair with: the App, its
// listener, and the two strings a pairing payload carries.
type pairedBackend struct {
	app         *App
	srv         *transport.Server
	fingerprint string
}

// newPairedBackend boots the identity core, mints this install's
// certificate, and serves the wire with every seam the real boot wires
// (main.go bootTransport). Loopback, because what this fixture is about is
// the CREDENTIAL rather than the peer: a test process cannot make the
// kernel report a LAN address for a connection it makes to itself, and
// the fabricated-peer listener that solves it is unexported. The other
// half — a non-loopback peer over pinned TLS, admitted on a ticket and on
// nothing else — is pinned beside that listener, in
// internal/transport's TestUpgradeAdmitsAPairedDeviceOffHostOverPinnedTLS.
func newPairedBackend(t *testing.T) *pairedBackend {
	t.Helper()
	app := identityApp(t)
	// Every minted payload names the backend, and a link that named none
	// would refuse to mint. Start publishes this from the store; a fixture
	// that builds the App directly has to do the same.
	storeIdentity, err := app.store.Identity()
	if err != nil {
		t.Fatalf("store identity: %v", err)
	}
	app.storeIdentity.Store(&storeIdentity)

	material, err := servercert.Load(t.TempDir())
	if err != nil {
		t.Fatalf("servercert.Load: %v", err)
	}
	SetCertFingerprint(app, material.Fingerprint)
	// The listener resolves its certificate per handshake out of this
	// holder, exactly as the boot's does — the fixture publishes the
	// self-signed half and nothing else, which is what a backend with no
	// canonical domain serves.
	certificateSource := transport.NewCertificateSource()
	certificateSource.SetSelfSigned(&material.Certificate)

	dispatcher := transport.NewDispatcher()
	// The same FQN labels the boot registers under, because a method's id
	// is hashed from them: registering the promoted receiver directly
	// under different labels would exercise a different method table than
	// the one production serves.
	if _, err := dispatcher.Register(app, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		t.Fatalf("register App methods: %v", err)
	}
	bus := transport.NewEventBus(64)
	app.SetEventBus(bus)

	srv, err := transport.New(transport.Config{
		Dispatcher:   dispatcher,
		EventBus:     bus,
		Token:        "launch-credential-under-test",
		Certificates: certificateSource,
		BackendIdentity: func() (string, string) {
			return BackendIdentity(app)
		},
		SessionForRequest: func(r *http.Request) (string, bool) {
			return SessionForRequest(app, r)
		},
		SessionLive:   func(sessionID string) bool { return SessionLive(app, sessionID) },
		SessionScopes: func(sessionID string) ([]string, string) { return SessionScopes(app, sessionID) },
		AuthEndpoints: AuthEndpoints(app),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	app.SetTransportServer(srv)
	AttachSessionConns(app, srv.SessionConns())
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &pairedBackend{app: app, srv: srv, fingerprint: material.Fingerprint}
}

// mintLink issues a pairing link the way the settings pane does and
// decodes it the way the terminal does.
func (b *pairedBackend) mintLink(t *testing.T, access string) (PairingInvite, deviceclient.Link) {
	t.Helper()
	invite, err := b.app.MintDevicePairing(string(identity.DeviceDesktop), access)
	if err != nil {
		t.Fatalf("MintDevicePairing: %v", err)
	}
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatalf("DecodeLink(%q): %v", invite.URL, err)
	}
	return invite, link
}

// TestAPairedDeviceAttachesOverPinnedTLS is the wave in one test, and
// every step is the real one:
//
// the owner mints a link, a Go-native device with no credential at all
// redeems it with a signed proof over its own request, the two screens
// show the same six digits, the owner confirms, the device observes its
// session go live, mints a socket ticket, dials wss with the certificate
// it pinned from the payload, is admitted on that ticket alone, and calls
// an RPC that answers.
//
// Nothing in that sequence is stubbed. A client that got any one of the
// proof's three easy details wrong, or dialled the cleartext half, or
// carried the session header instead of a ticket, fails here.
func TestAPairedDeviceAttachesOverPinnedTLS(t *testing.T) {
	backend := newPairedBackend(t)
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))

	if link.CertFingerprint != backend.fingerprint {
		t.Fatalf("the payload names %q, the listener presents %q", link.CertFingerprint, backend.fingerprint)
	}
	if !strings.HasPrefix(link.Endpoint, "http://") {
		t.Fatalf("endpoint %q: the payload names the cleartext authority a browser uses", link.Endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile := t.TempDir()
	client, pairing, err := deviceclient.Pair(ctx, profile, link, "integration device", "linux")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	// Promoted to https by the fingerprint alone: the same port answers
	// both halves, and a payload carrying a fingerprint is one whose
	// backend terminates TLS on that authority.
	if !strings.HasPrefix(client.Endpoint(), "https://") {
		t.Fatalf("the client dials %q, want the TLS half", client.Endpoint())
	}

	// The two screens. The owner's number is derived from the key the
	// device actually presented, which is what makes a mismatch mean
	// "some other device redeemed this link".
	status, err := backend.app.DevicePairingStatus(invite.LinkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus: %v", err)
	}
	if status.VerificationNumber == "" || status.VerificationNumber != pairing.VerificationNumber {
		t.Fatalf("the owner sees %q and the device shows %q",
			status.VerificationNumber, pairing.VerificationNumber)
	}

	// Inert until the owner confirms: the credential is real and every
	// presentation refuses.
	if _, err := client.Ticket(ctx); err == nil {
		t.Fatal("an unconfirmed session minted a socket ticket")
	}

	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("ConfirmDevicePairing: %v", err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}

	ticket, err := client.Ticket(ctx)
	if err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	dialURL, err := client.DialURL(ticket)
	if err != nil {
		t.Fatalf("DialURL: %v", err)
	}
	if !strings.HasPrefix(dialURL, "wss://") {
		t.Fatalf("the socket URL is %q, want the same half of the listener the rest rode", dialURL)
	}

	conn, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: client.RoundTripper()},
	})
	if err != nil {
		t.Fatalf("dial the pinned socket with a spent ticket: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// The upgrade carried no launch credential. It was admitted because a
	// spent ticket both names a live session and stands in for one.
	projects := callOverWS(t, ctx, conn, "ListProjects")
	var listed []json.RawMessage
	if err := json.Unmarshal(projects, &listed); err != nil {
		t.Fatalf("decode ListProjects result %s: %v", projects, err)
	}
	if len(listed) == 0 {
		t.Fatal("ListProjects answered an empty list; the fixture seeds one project")
	}

	// And what it holds is what the link granted, published on the
	// redemption so the client can say what it may offer.
	if len(pairing.Scopes) == 0 {
		t.Fatal("the redemption published no scopes")
	}
}

// TestAViewOnlyLinkGrantsOnlyWhatItNamed — narrowing is chosen at mint and
// is the session's for its whole life, so a device paired with a view-only
// link is refused an execute-tier call by the per-RPC gate rather than by
// anything this client decides for itself.
func TestAViewOnlyLinkGrantsOnlyWhatItNamed(t *testing.T) {
	backend := newPairedBackend(t)
	invite, link := backend.mintLink(t, string(identity.PairingAccessViewOnly))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, _, err := deviceclient.Pair(ctx, t.TempDir(), link, "view only", "linux")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("ConfirmDevicePairing: %v", err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}

	conn := dialPaired(t, ctx, client)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// threads:read is in the observe tier a view-only link grants.
	if _, err := callOverWSErr(t, ctx, conn, "ListProjects"); err != nil {
		t.Fatalf("a view-only session was refused an observe-tier call: %v", err)
	}
	// access:admin is not, and the gate names what is missing.
	_, err = callOverWSErr(t, ctx, conn, "GetAccessOverview")
	if err == nil {
		t.Fatal("a view-only session reached the device-administration surface")
	}
	var refusal *wireError
	if !errors.As(err, &refusal) || refusal.Code != transport.ErrCodeScopeRequired {
		t.Fatalf("refusal = %v, want a typed scope_required", err)
	}
	if refusal.Scope == "" {
		t.Error("the refusal names no scope, so a client cannot explain the disabled surface")
	}
}

// TestAFingerprintMismatchRefusesBeforeAnyCredentialLeaves is the property
// the pairing ceremony bought. The pin is compared during the handshake,
// so a backend presenting other bytes never receives this device's link
// token, its proof, or its session credential.
func TestAFingerprintMismatchRefusesBeforeAnyCredentialLeaves(t *testing.T) {
	backend := newPairedBackend(t)
	_, link := backend.mintLink(t, string(identity.PairingAccessFull))
	// Same endpoint, a fingerprint that is not this listener's.
	link.CertFingerprint = servercert.Fingerprint(otherCertificate(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	profile := t.TempDir()
	_, _, err := deviceclient.Pair(ctx, profile, link, "mismatched", "linux")
	if !errors.Is(err, deviceclient.ErrCertificateMismatch) {
		t.Fatalf("Pair against a mismatched pin = %v, want ErrCertificateMismatch", err)
	}
	if _, err := deviceclient.LoadSession(profile, link.BackendID); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("a refused pairing left a session behind: %v", err)
	}
	// The link was never presented, so the owner's link is still theirs to
	// spend on a device that CAN verify this backend.
	if _, _, err := deviceclient.Pair(ctx, profile, mustPin(t, link, backend.fingerprint), "corrected", "linux"); err != nil {
		t.Fatalf("the mismatched attempt spent the link: %v", err)
	}
}

// TestARevokedDeviceIsRefusedAndTheClientNamesTheRemedy — revocation is
// absolute: the session's own row and its device's row are both consulted
// on every call, so revoking the DEVICE ends the session it holds. What
// the client owes the person is the one thing that works, which is pairing
// again — and it must not sit in a retry loop instead.
func TestARevokedDeviceIsRefusedAndTheClientNamesTheRemedy(t *testing.T) {
	backend := newPairedBackend(t)
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile := t.TempDir()
	client, _, err := deviceclient.Pair(ctx, profile, link, "to be revoked", "linux")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("ConfirmDevicePairing: %v", err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}

	overview, err := backend.app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	revoked := 0
	for _, device := range overview.Devices {
		if device.Label != "to be revoked" {
			continue
		}
		if _, err := backend.app.RevokeAccessDevice(device.ID); err != nil {
			t.Fatalf("RevokeAccessDevice: %v", err)
		}
		revoked++
	}
	if revoked != 1 {
		t.Fatalf("the overview holds %d matching devices, want the one that paired", revoked)
	}

	_, err = client.Ticket(ctx)
	if !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("a revoked device's mint = %v, want ErrSessionEnded", err)
	}
	if !strings.Contains(err.Error(), "pair this device again") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
	// Cleared client-side too, so nothing retries something that can never
	// succeed. The key stays: it names the device, and the backend adopts
	// its row by thumbprint when this installation pairs again.
	if _, err := deviceclient.LoadSession(profile, link.BackendID); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("the revoked session survived client-side: %v", err)
	}
	if _, err := deviceclient.DeviceKey(profile); err != nil {
		t.Fatalf("the device key did not survive a revocation: %v", err)
	}
}

// TestRefreshReuseEndsTheSessionOnBothSides — presenting a spent refresh
// secret is the leaked-copy detector, and it is deliberately unable to
// tell a copy from the original, so BOTH stop. This client's job is to
// notice and clear, rather than to keep presenting a credential the
// backend has already ended.
func TestRefreshReuseEndsTheSessionOnBothSides(t *testing.T) {
	backend := newPairedBackend(t)
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile := t.TempDir()
	client, _, err := deviceclient.Pair(ctx, profile, link, "reuse", "linux")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("ConfirmDevicePairing: %v", err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}

	// Two clients over the SAME stored session, both holding the secret
	// one of them is about to spend. That is what a copied profile
	// directory looks like from the backend's side — and the credential is
	// aged past its margin so each one rotates before it does anything
	// else, which is the ordinary path rather than a forced one.
	held := client.Session()
	held.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	if err := deviceclient.SaveSession(profile, held); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	original, err := deviceclient.Open(profile, held)
	if err != nil {
		t.Fatalf("Open the original: %v", err)
	}
	copied, err := deviceclient.Open(profile, held)
	if err != nil {
		t.Fatalf("Open the copy: %v", err)
	}

	if _, err := original.Ticket(ctx); err != nil {
		t.Fatalf("the first rotation failed: %v", err)
	}
	if _, err := copied.Ticket(ctx); !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("presenting a spent secret = %v, want ErrSessionEnded", err)
	}

	// The family is finished, so the rotated half is refused too — the
	// detector cannot tell which copy leaked, and neither can this test.
	if _, err := original.Ticket(ctx); !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("the rotated credential = %v, want ErrSessionEnded after reuse", err)
	}
	if _, err := deviceclient.LoadSession(profile, link.BackendID); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("the ended session survived client-side: %v", err)
	}
}

// dialPaired mints a ticket and dials the pinned socket with it, which is
// the whole of how a paired device is admitted.
func dialPaired(t *testing.T, ctx context.Context, client *deviceclient.Client) *websocket.Conn {
	t.Helper()
	ticket, err := client.Ticket(ctx)
	if err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	dialURL, err := client.DialURL(ticket)
	if err != nil {
		t.Fatalf("DialURL: %v", err)
	}
	conn, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: client.RoundTripper()},
	})
	if err != nil {
		t.Fatalf("dial the pinned socket: %v", err)
	}
	return conn
}

// wireError is a frame error a caller can branch on, which is what the
// spec requires of a refusal a non-loopback client has to explain.
type wireError struct {
	Code    string
	Message string
	Scope   string
}

func (e *wireError) Error() string {
	if e.Scope != "" {
		return e.Code + " (" + e.Scope + "): " + e.Message
	}
	return e.Code + ": " + e.Message
}

// callOverWS makes one RPC and fails the test on a refusal.
func callOverWS(t *testing.T, ctx context.Context, conn *websocket.Conn, method string) json.RawMessage {
	t.Helper()
	result, err := callOverWSErr(t, ctx, conn, method)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return result
}

// callOverWSErr makes one RPC and returns the typed refusal, skipping the
// frames that are not this call's answer — hello, pings, pushed events.
func callOverWSErr(t *testing.T, ctx context.Context, conn *websocket.Conn, method string) (json.RawMessage, error) {
	t.Helper()
	request, err := json.Marshal(transport.ClientFrame{Type: "rpc", ID: method, Method: method})
	if err != nil {
		t.Fatalf("encode the call: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatalf("write the call: %v", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read the answer: %v", err)
		}
		var frame transport.ServerFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if frame.Type != "rpc" || frame.ID != method {
			continue
		}
		if frame.Error != nil {
			return nil, &wireError{Code: frame.Error.Code, Message: frame.Error.Message, Scope: frame.Error.Scope}
		}
		return frame.Result, nil
	}
}

// mustPin returns the link with the fingerprint corrected, for the case
// that is about the pin rather than about the link.
func mustPin(t *testing.T, link deviceclient.Link, fingerprint string) deviceclient.Link {
	t.Helper()
	link.CertFingerprint = fingerprint
	return link
}

// otherCertificate is a valid self-signed certificate that is not this
// backend's, which is the only shape the mismatch case needs.
func otherCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "another backend"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	return der
}
