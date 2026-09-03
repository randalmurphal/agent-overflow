package acmecert

import (
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
	"testing"
	"time"
)

// certificateFor builds a certificate valid for one name over a given
// window. It stands in for whatever a certificate authority issued; the
// renewal decision reads only the leaf.
func certificateFor(t *testing.T, name string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func materialFor(t *testing.T, name string, life time.Duration) Material {
	t.Helper()
	now := time.Now()
	material, err := materialFrom(certificateFor(t, name, now.Add(-time.Hour), now.Add(life)))
	if err != nil {
		t.Fatalf("materialFrom: %v", err)
	}
	return material
}

// One predicate decides "due", so the boot check, the daily tick and the
// manual button cannot disagree.
func TestNeedsRenewal(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		material Material
		domain   string
		want     bool
	}{
		{
			name:     "nothing issued yet",
			material: Material{},
			domain:   "backend.example",
			want:     true,
		},
		{
			name:     "issued for the domain the user configured before this one",
			material: materialFor(t, "old.example", 60*24*time.Hour),
			domain:   "backend.example",
			want:     true,
		},
		{
			name:     "fresh, two months left",
			material: materialFor(t, "backend.example", 60*24*time.Hour),
			domain:   "backend.example",
			want:     false,
		},
		{
			name:     "a day outside the window",
			material: materialFor(t, "backend.example", RenewWindow+24*time.Hour),
			domain:   "backend.example",
			want:     false,
		},
		{
			name:     "a day inside the window",
			material: materialFor(t, "backend.example", RenewWindow-24*time.Hour),
			domain:   "backend.example",
			want:     true,
		},
		{
			name:     "already expired",
			material: materialFor(t, "backend.example", -time.Hour),
			domain:   "backend.example",
			want:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.material.NeedsRenewal(test.domain, now); got != test.want {
				t.Fatalf("NeedsRenewal = %v, want %v", got, test.want)
			}
		})
	}
}

// A backend that has never issued one is not a backend in an error state.
func TestLoadAnswersEmptyWhenNothingWasEverIssued(t *testing.T) {
	material, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load with no certificate present: %v", err)
	}
	if material.Loaded() {
		t.Fatal("Load invented a certificate")
	}
}

// Unusable material is reported rather than replaced: re-issuing over it
// would spend an issuance against the authority's rate limit every time a
// truncated file was read.
func TestLoadReportsMaterialItCannotUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CertFileName), []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("a certificate file that does not parse loaded without complaint")
	}
}

// The private-CA escape hatch: two files, the shape every outside tool
// already writes.
func TestLoadPairReadsAnExternalCertificate(t *testing.T) {
	dir := t.TempDir()
	cert := certificateFor(t, "backend.example", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	certFile := filepath.Join(dir, "fullchain.pem")
	keyFile := filepath.Join(dir, "privkey.pem")
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	material, err := LoadPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if !material.Covers("backend.example") {
		t.Fatal("the loaded pair is not valid for the domain it names")
	}

	if _, err := LoadPair(certFile, ""); err == nil {
		t.Fatal("half a pair loaded")
	}
	if _, err := LoadPair(filepath.Join(dir, "absent.pem"), keyFile); err == nil {
		t.Fatal("a missing certificate file loaded")
	}
}
