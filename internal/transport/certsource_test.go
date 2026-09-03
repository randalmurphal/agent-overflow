package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/servercert"
)

// selfSignedFor mints a certificate for one DNS name. Not
// internal/servercert's: this stands in for the DOMAIN certificate, which
// in production comes from Let's Encrypt or the user's own CA, and the
// only property these tests need from it is that it is a different
// certificate carrying a different name.
func selfSignedFor(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign %s: %v", name, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// presentedLeaf completes one handshake against the listener with the
// given SNI and reports the certificate it was answered with. It verifies
// nothing: which certificate arrives is the whole subject here.
func presentedLeaf(t *testing.T, addr, serverName string) *x509.Certificate {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("handshake with SNI %q: %v", serverName, err)
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		t.Fatalf("SNI %q was answered with no certificate", serverName)
	}
	return chain[0]
}

// The SNI split is what keeps both audiences served by one port: the name
// a browser was given gets the certificate a trust store accepts, and
// EVERYTHING else gets the bytes a paired client pinned.
func TestSNIDecidesWhichCertificateAnswers(t *testing.T) {
	srv, material, source := tlsFixtureWithSource(t, nil)
	domain := selfSignedFor(t, "backend.example")
	source.SetDomain("backend.example", &domain)

	if got := presentedLeaf(t, srv.Addr(), "backend.example"); got.Subject.CommonName != "backend.example" {
		t.Fatalf("the canonical domain was answered with %q", got.Subject.CommonName)
	}
	// Case-insensitively, because SNI is a DNS name and a client may spell
	// it however it likes.
	if got := presentedLeaf(t, srv.Addr(), "Backend.Example"); got.Subject.CommonName != "backend.example" {
		t.Fatalf("a differently-cased spelling of the domain was answered with %q", got.Subject.CommonName)
	}
	for _, name := range []string{"localhost", "something.else", ""} {
		got := presentedLeaf(t, srv.Addr(), name)
		if fingerprint := servercert.Fingerprint(got.Raw); fingerprint != material.Fingerprint {
			t.Fatalf("SNI %q was not answered with the self-signed certificate a paired client pinned (subject %q)",
				name, got.Subject.CommonName)
		}
	}
}

// A renewal must not need a rebind. The listener is the one a paired
// client is connected to; swapping it to install a certificate would drop
// every one of them, on a schedule nobody is watching.
func TestANewCertificateServesWithoutARebind(t *testing.T) {
	srv, _, source := tlsFixtureWithSource(t, nil)
	first := selfSignedFor(t, "backend.example")
	source.SetDomain("backend.example", &first)
	before := presentedLeaf(t, srv.Addr(), "backend.example").SerialNumber

	renewed := selfSignedFor(t, "backend.example")
	source.SetDomain("backend.example", &renewed)

	after := presentedLeaf(t, srv.Addr(), "backend.example").SerialNumber
	if before.Cmp(after) == 0 {
		t.Fatal("the renewed certificate did not take effect on the next handshake")
	}
	if after.Cmp(renewed.Leaf.SerialNumber) != 0 {
		t.Fatal("the handshake was answered with neither the old certificate nor the new one")
	}

	// Clearing it puts the self-signed certificate back on every name,
	// which is the state a user who removed their domain is left in.
	source.SetDomain("", nil)
	if got := presentedLeaf(t, srv.Addr(), "backend.example"); got.Subject.CommonName == "backend.example" {
		t.Fatal("a cleared domain certificate is still being presented")
	}
}

// A source holding nothing yet is the ordinary state of a boot whose
// certificate has not resolved. The handshake is refused; the listener
// keeps serving cleartext, and a certificate arriving later serves
// without a restart.
func TestAHandshakeBeforeAnyCertificateIsRefusedAndRecovers(t *testing.T) {
	source := NewCertificateSource()
	srv := newServerFixtureWith(t, func(cfg *Config) { cfg.Certificates = source }).srv

	if _, err := tls.Dial("tcp", srv.Addr(), &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}); err == nil {
		t.Fatal("a handshake completed with no certificate loaded")
	}

	// The cleartext half is unaffected — that is what a browser is on.
	resp, err := http.Get("http://" + srv.Addr() + HealthPath)
	if err != nil {
		t.Fatalf("cleartext request while no certificate is loaded: %v", err)
	}
	_ = resp.Body.Close()

	late := selfSignedFor(t, "localhost")
	source.SetSelfSigned(&late)
	if got := presentedLeaf(t, srv.Addr(), "localhost"); got.SerialNumber.Cmp(late.Leaf.SerialNumber) != 0 {
		t.Fatal("a certificate installed after the bind did not serve")
	}
}
