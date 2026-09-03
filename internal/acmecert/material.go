package acmecert

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
)

// CertFileName is the issued certificate and its key, one combined PEM
// under the app's config root. One file for the reason internal/servercert
// keeps one: the chain and the key are only useful together, and two
// files can be torn relative to each other even when each write is
// atomic.
const CertFileName = "acme-cert.pem"

// AccountFileName is the ACME account key. Separate from the certificate
// because it has a different lifetime: the certificate is replaced every
// renewal, the account key is the identity every renewal is made UNDER
// and outlives all of them.
const AccountFileName = "acme-account.pem"

// RenewWindow is how much life must be left before a certificate is
// renewed. Let's Encrypt issues for 90 days and asks for renewal at 30,
// which leaves a month of daily attempts before anything a user would
// notice — enough that a DNS hook broken by an unrelated change is found
// by a status line rather than by an outage.
const RenewWindow = 30 * 24 * time.Hour

// Material is an issued certificate as the listener wants it.
type Material struct {
	// Certificate is what the listener presents for the canonical
	// domain, chain included. Leaf is populated.
	Certificate tls.Certificate

	// NotAfter is the leaf's expiry, lifted out so the renewal decision
	// and the settings status do not each re-parse the DER.
	NotAfter time.Time
}

// Loaded reports whether this Material holds a certificate at all. The
// zero value is the ordinary answer for a backend with no domain
// configured, so callers ask rather than comparing against nil fields.
func (m Material) Loaded() bool { return m.Certificate.Leaf != nil }

// Covers reports whether this certificate is valid for the given name.
// A stored certificate for the domain the user configured LAST WEEK is
// not a certificate for the domain they configured today, and serving it
// under the new name would fail in the browser rather than here.
func (m Material) Covers(domain string) bool {
	if !m.Loaded() {
		return false
	}
	return m.Certificate.Leaf.VerifyHostname(strings.TrimSpace(domain)) == nil
}

// NeedsRenewal reports whether an issuance should run now. Answers true
// for material that is absent, does not cover the domain, or is inside
// RenewWindow of expiry — one predicate, so the boot check, the daily
// tick and the manual button cannot disagree about what "due" means.
func (m Material) NeedsRenewal(domain string, now time.Time) bool {
	if !m.Covers(domain) {
		return true
	}
	return now.After(m.NotAfter.Add(-RenewWindow))
}

// Load reads the issued certificate kept in dir. A missing file is not an
// error: it is the state of every install that has never issued one, and
// the caller's answer to it is to issue rather than to fail.
//
// Material that is present but unusable IS an error, deliberately unlike
// internal/servercert, which re-mints over it. There the file is an
// identity this backend owns and can replace at will; here it is the
// product of a rate-limited exchange with a certificate authority, and
// silently discarding it would spend an issuance against the CA's
// weekly limit every time a disk hiccup truncated the file.
func Load(dir string) (Material, error) {
	if strings.TrimSpace(dir) == "" {
		return Material{}, errors.New("acmecert: no directory to read the certificate from")
	}
	path := filepath.Join(dir, CertFileName)
	stored, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Material{}, nil
		}
		return Material{}, fmt.Errorf("acmecert: read %s: %w", path, err)
	}
	return decode(stored, path)
}

// LoadPair reads a certificate and key the user supplied themselves —
// the private-CA escape hatch, or a pair some outside tool (certbot,
// mkcert) renews. Two files rather than one because that is the shape
// every such tool already writes.
func LoadPair(certFile, keyFile string) (Material, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" {
		return Material{}, errors.New("acmecert: an external certificate needs both a certificate file and a key file")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: load the certificate %s with the key %s: %w", certFile, keyFile, err)
	}
	return materialFrom(cert)
}

// Persist writes the certificate and its key to dir as one 0600 PEM,
// certificate chain first so a human running `openssl x509 -in` on it
// gets the interesting half.
func Persist(dir string, cert tls.Certificate) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("acmecert: no directory to persist the certificate to")
	}
	encoded, err := encodeCertificate(cert)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, CertFileName)
	if err := atomicfile.Write(path, encoded); err != nil {
		return fmt.Errorf("acmecert: persist %s: %w", path, err)
	}
	return nil
}

// decode turns the stored combined PEM into Material. The certificate and
// the key come out of the same bytes: tls.X509KeyPair skips the blocks
// each half does not want, which is what lets one file hold both.
func decode(stored []byte, path string) (Material, error) {
	cert, err := tls.X509KeyPair(stored, stored)
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: %s does not hold a usable certificate and key: %w", path, err)
	}
	return materialFrom(cert)
}

// tlsCertificate assembles what a CA returned into the shape the
// listener serves. The chain arrives leaf-first, which is also the order
// crypto/tls presents it in.
func tlsCertificate(chain [][]byte, key crypto.PrivateKey) tls.Certificate {
	return tls.Certificate{Certificate: chain, PrivateKey: key}
}

// encodeCertificate renders a certificate and its key as the combined PEM
// this package persists: every certificate in the chain, then the key.
// The key half is what makes a failure here worth reporting rather than
// ignoring — a file holding a chain and no key is one the next boot
// cannot load, which reads as "never issued" and orders again.
func encodeCertificate(cert tls.Certificate) ([]byte, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("acmecert: the certificate carries no chain")
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("acmecert: a %T cannot sign, so it is not a usable certificate key", cert.PrivateKey)
	}
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := encodeKey(signer)
	if err != nil {
		return nil, err
	}
	return append(out, key...), nil
}

func materialFrom(cert tls.Certificate) (Material, error) {
	if len(cert.Certificate) == 0 {
		return Material{}, errors.New("acmecert: the certificate carries no chain")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: parse the leaf certificate: %w", err)
	}
	cert.Leaf = leaf
	return Material{Certificate: cert, NotAfter: leaf.NotAfter}, nil
}
