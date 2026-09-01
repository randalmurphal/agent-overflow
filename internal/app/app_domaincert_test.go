package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
)

// recordingSource stands in for the transport's certificate holder. The
// reconciler's whole output is what it publishes here.
type recordingSource struct {
	name string
	cert *tls.Certificate
}

func (r *recordingSource) SetDomain(name string, cert *tls.Certificate) {
	r.name = name
	r.cert = cert
}

// writeExternalPair writes a certificate and key for one name, the shape
// every outside tool already produces.
func writeExternalPair(t *testing.T, dir, name string, life time.Duration) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(11),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(life),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certFile = filepath.Join(dir, name+".crt")
	keyFile = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

// domainCertApp is an App with settings and a config root, and no
// transport: the reconciler publishes into whatever source it is handed,
// so the certificate half is testable without a listener.
func domainCertApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	app := &App{settings: settings.NewService(dir)}
	app.domainCert.dir = dir
	return app, dir
}

// The escape hatch, end to end: the user's own certificate is loaded,
// published for the canonical domain, and reported as external.
func TestAnExternalPairIsServedForTheCanonicalDomain(t *testing.T) {
	app, dir := domainCertApp(t)
	certFile, keyFile := writeExternalPair(t, dir, "backend.example", 90*24*time.Hour)
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{
		CanonicalDomain:  "backend.example",
		ExternalCertFile: certFile,
		ExternalKeyFile:  keyFile,
	}); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	source := &recordingSource{}

	if wait := app.reconcileExternalCertificate(app.settings.Get().Network, source); wait != domainCertCheckInterval {
		t.Fatalf("next check in %s, want the quiet cadence", wait)
	}
	if source.name != "backend.example" || source.cert == nil {
		t.Fatalf("published %q / %v, want the certificate under the canonical domain", source.name, source.cert)
	}

	status := app.domainCertStatus()
	if status.Serving != network.TLSServingExternal {
		t.Fatalf("serving = %q, want %q", status.Serving, network.TLSServingExternal)
	}
	if status.NotAfter == 0 {
		t.Fatal("no expiry was reported for a loaded certificate")
	}
	if status.LastError != "" {
		t.Fatalf("last error = %q, want none", status.LastError)
	}
}

// An external pair that does not cover the name is a configuration
// mistake the user has to be told about, not a certificate to serve
// under a name it is not valid for.
func TestAnExternalPairForAnotherNameIsRefusedAndReported(t *testing.T) {
	app, dir := domainCertApp(t)
	certFile, keyFile := writeExternalPair(t, dir, "somewhere.else", 90*24*time.Hour)
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{
		CanonicalDomain:  "backend.example",
		ExternalCertFile: certFile,
		ExternalKeyFile:  keyFile,
	}); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	source := &recordingSource{}

	app.reconcileExternalCertificate(app.settings.Get().Network, source)
	if source.cert != nil {
		t.Fatal("a certificate for another name was published")
	}
	status := app.domainCertStatus()
	if status.Serving != network.TLSServingNone {
		t.Fatalf("serving = %q, want %q with nothing loaded", status.Serving, network.TLSServingNone)
	}
	if !strings.Contains(status.LastError, "not valid for backend.example") {
		t.Fatalf("last error = %q, want it to name the mismatch", status.LastError)
	}
}

// The second look at unchanged files does not re-read them, and a file
// the user's renewal tool rewrote does.
func TestAnExternalPairIsRereadOnlyWhenItChanges(t *testing.T) {
	app, dir := domainCertApp(t)
	certFile, keyFile := writeExternalPair(t, dir, "backend.example", 90*24*time.Hour)
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{
		CanonicalDomain:  "backend.example",
		ExternalCertFile: certFile,
		ExternalKeyFile:  keyFile,
	}); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	cfg := app.settings.Get().Network
	source := &recordingSource{}

	app.reconcileExternalCertificate(cfg, source)
	first := source.cert
	if first == nil {
		t.Fatal("nothing was published on the first pass")
	}

	source.cert = nil
	app.reconcileExternalCertificate(cfg, source)
	if source.cert != nil {
		t.Fatal("unchanged files were re-read and re-published")
	}

	// A renewal by an outside tool: same paths, different bytes.
	replacement, replacementKey := writeExternalPair(t, t.TempDir(), "backend.example", 30*24*time.Hour)
	copyFile(t, replacement, certFile)
	copyFile(t, replacementKey, keyFile)
	app.reconcileExternalCertificate(cfg, source)
	if source.cert == nil {
		t.Fatal("a rewritten certificate was not picked up")
	}
	if source.cert.Leaf.NotAfter.Equal(first.Leaf.NotAfter) {
		t.Fatal("the reloaded certificate is the old one")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	// A stamp is (size, modtime), and two writes inside one filesystem
	// timestamp tick would stamp the same. The replacement differs in
	// size, but pin the modtime too so the test cannot depend on that.
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(to, later, later); err != nil {
		t.Fatalf("chtimes %s: %v", to, err)
	}
}

// With no domain configured there is nothing to serve for one, and the
// status says what the listener actually presents.
func TestNoCanonicalDomainClearsTheDomainCertificate(t *testing.T) {
	app, _ := domainCertApp(t)
	source := &recordingSource{name: "stale.example", cert: &tls.Certificate{}}

	app.publishDomainCertificate(source, "", "", tls.Certificate{}, time.Time{})
	if source.name != "" || source.cert != nil {
		t.Fatalf("the domain certificate was left installed: %q / %v", source.name, source.cert)
	}
	if got := app.domainCertStatus().Serving; got != network.TLSServingNone {
		t.Fatalf("serving = %q, want %q with no certificate at all", got, network.TLSServingNone)
	}

	// With the install's own certificate resolved at boot, the same
	// state reads as self-signed: that is what a client meets.
	app.certFingerprint = "sha256:abc"
	if got := app.domainCertStatus().Serving; got != network.TLSServingSelfSigned {
		t.Fatalf("serving = %q, want %q", got, network.TLSServingSelfSigned)
	}
}

// Failures back off, so a broken hook does not order once a minute
// against the authority's rate limit, and the delay is bounded.
func TestIssuanceFailuresBackOffAndAreBounded(t *testing.T) {
	app, _ := domainCertApp(t)
	var previous time.Duration
	for i := 0; i < 12; i++ {
		app.recordDomainCertFailure("the hook could not publish the record")
		delay := app.domainCertRetryDelay()
		if delay < domainCertRetryFloor || delay > domainCertRetryCeiling {
			t.Fatalf("retry %d in %s, want between %s and %s", i, delay, domainCertRetryFloor, domainCertRetryCeiling)
		}
		if delay < previous {
			t.Fatalf("retry %d in %s, shorter than the previous %s", i, delay, previous)
		}
		previous = delay
	}
	if previous != domainCertRetryCeiling {
		t.Fatalf("the backoff settled at %s, want the ceiling %s", previous, domainCertRetryCeiling)
	}

	// A success clears both the message and the count.
	app.publishDomainCertificate(nil, "backend.example", network.TLSServingACME, tls.Certificate{}, time.Now().Add(time.Hour))
	if status := app.domainCertStatus(); status.LastError != "" {
		t.Fatalf("last error = %q after a success, want none", status.LastError)
	}
	if got := app.domainCertRetryDelay(); got != domainCertRetryFloor {
		t.Fatalf("retry delay = %s after a success, want the floor %s", got, domainCertRetryFloor)
	}
}

// The reconciler must never order a certificate for a domain the user
// already holds one for: the external pair wins, and issuance is not
// reached at all.
func TestTheExternalPairWinsOverIssuance(t *testing.T) {
	app, dir := domainCertApp(t)
	certFile, keyFile := writeExternalPair(t, dir, "backend.example", 90*24*time.Hour)
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{
		CanonicalDomain:  "backend.example",
		ACMEDNSHook:      []string{"/bin/false"},
		ExternalCertFile: certFile,
		ExternalKeyFile:  keyFile,
	}); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}

	app.reconcileDomainCertificate(context.Background())

	status := app.domainCertStatus()
	if status.Serving != network.TLSServingExternal {
		t.Fatalf("serving = %q, want %q", status.Serving, network.TLSServingExternal)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme-account.pem")); err == nil {
		t.Fatal("an ACME account was registered for a domain that already had a certificate")
	}
}
