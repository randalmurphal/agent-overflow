// Package servercert mints, persists and reloads the self-signed TLS
// certificate the backend terminates its own listener with
// (docs/specs/remote-access.md §7, "Domainless TLS for Go-native
// clients").
//
// Nothing is ever asked to TRUST this certificate. No CA signs it, no
// browser accepts it, and no step here tells a person to install
// anything. Its value is that the pairing payload carries its
// fingerprint, so a client that owns its own TLS configuration — the
// desktop attach client, the CLI, the phone shell's native bridge —
// pins these exact bytes. The pairing ceremony that already establishes
// trust is what anchors the channel, which is how a LAN with no domain
// and no CA still gets an encrypted, authenticated one.
//
// Ten-year validity, because a pinning client compares bytes and never
// consults a date: a browser refuses this certificate whatever its
// expiry says, and rotation rides an established session as a signed
// successor announcement rather than as a deadline nobody scheduled. A
// short life would only buy a day when every paired device stops
// connecting.
//
// Re-minting is the one loud event here. The fingerprint is what paired
// devices pinned, so replacing material this package cannot read un-pins
// every one of them — Load says so, in the log and in its answer, rather
// than quietly handing the backend a new identity.
package servercert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
)

// FileName is the combined PEM file under the app's config root. One
// file rather than two, because the certificate and the key are only
// ever useful together: two files can be torn relative to each other
// even when each write is atomic, and a half-updated pair is exactly the
// state that would un-pin every device on the next boot.
const FileName = "server-cert.pem"

// Validity is how long a minted certificate is dated for. See the
// package doc for why it is not shorter.
const Validity = 10 * 365 * 24 * time.Hour

// clockSkew backdates NotBefore so a device whose clock runs a little
// ahead of this machine's does not see a certificate from the future on
// the first connection after a mint.
const clockSkew = time.Hour

// fingerprintPrefix names the digest in the pairing payload's
// fingerprint string. It travels on the wire, so the spelling is a
// contract with every client that pins, not a log format.
const fingerprintPrefix = "sha256:"

// commonName is what the certificate calls itself. Cosmetic: a pinning
// client matches bytes, and nothing else ever reads this.
const commonName = "Agent Overflow backend"

// Material is the certificate this backend presents, plus the string the
// pairing payload carries for it and what Load had to do to produce it.
type Material struct {
	// Certificate is what the listener serves. Leaf is populated, so a
	// caller can read the dates without re-parsing the DER.
	Certificate tls.Certificate

	// Fingerprint is "sha256:<lowercase hex>" over the DER of the leaf —
	// the exact string the pairing payload carries and a pinning client
	// compares against.
	Fingerprint string

	// Minted is true when this call created the certificate rather than
	// loading one. A first boot sets it; so does every re-mint.
	Minted bool

	// Replaced says why existing material was discarded, and is empty
	// when there was none to discard. Non-empty means the fingerprint
	// changed: every device paired against the previous certificate
	// pinned a value this backend no longer presents, and has to pair
	// again. Callers surface it; they never treat it as routine.
	Replaced string
}

// Load returns the certificate kept in dir, minting and persisting one
// when there is none — or when what is there cannot be used.
//
// Unreadable material is overwritten rather than made fatal: a backend
// that refused to start because a file it owns was truncated by a
// half-written disk would be unreachable for a reason the user cannot
// act on, while a re-mint costs them one re-pair per device and is
// announced.
func Load(dir string) (Material, error) {
	if strings.TrimSpace(dir) == "" {
		return Material{}, errors.New("servercert: no directory to keep the certificate in")
	}
	path := filepath.Join(dir, FileName)
	stored, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A read that failed for a reason other than absence (a
			// permission the app cannot fix, an I/O error) is not cured
			// by writing over it, and doing so would destroy material
			// that may still be readable once the cause is gone.
			return Material{}, fmt.Errorf("servercert: read %s: %w", path, err)
		}
		return mintTo(path, time.Now())
	}

	material, why := usableMaterial(stored, time.Now())
	if why == nil {
		return material, nil
	}
	minted, mintErr := mintTo(path, time.Now())
	if mintErr != nil {
		return Material{}, mintErr
	}
	minted.Replaced = why.Error()
	log.Printf("servercert: %s could not be used (%v) — minted a replacement, fingerprint %s. "+
		"Devices paired against the previous certificate pinned a value this backend no longer "+
		"presents and have to pair again.", path, why, minted.Fingerprint)
	return minted, nil
}

// usableMaterial parses stored PEM and reports why it cannot be served,
// or nil when it can. The certificate and the key are read from the same
// bytes: tls.X509KeyPair skips the blocks each half does not want, which
// is what lets one file hold both.
func usableMaterial(stored []byte, now time.Time) (Material, error) {
	cert, err := tls.X509KeyPair(stored, stored)
	if err != nil {
		return Material{}, fmt.Errorf("the certificate and key do not load: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Material{}, fmt.Errorf("the certificate does not parse: %w", err)
	}
	if now.After(leaf.NotAfter) {
		return Material{}, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format(time.RFC3339))
	}
	// NotBefore is deliberately NOT checked. A machine whose clock reads
	// earlier than the mint is a clock problem, and re-minting would
	// answer it by un-pinning every paired device — a permanent cost for
	// a condition that fixes itself on the next time sync.
	cert.Leaf = leaf
	return Material{Certificate: cert, Fingerprint: Fingerprint(cert.Certificate[0])}, nil
}

// mintTo mints a certificate and writes it to path before returning it,
// so the fingerprint this process publishes is the one the next boot
// will find. A write failure is fatal to the call for that reason: a
// certificate held only in memory would re-mint on every restart, and
// every pairing done against it would stop working at the next one.
func mintTo(path string, now time.Time) (Material, error) {
	cert, err := mint(now)
	if err != nil {
		return Material{}, err
	}
	encoded, err := encode(cert)
	if err != nil {
		return Material{}, err
	}
	if err := atomicfile.Write(path, encoded); err != nil {
		return Material{}, fmt.Errorf("servercert: persist %s: %w", path, err)
	}
	return Material{
		Certificate: cert,
		Fingerprint: Fingerprint(cert.Certificate[0]),
		Minted:      true,
	}, nil
}

// mint generates the key and self-signs it.
//
// ECDSA P-256: every client that will pin this is a Go process, a
// WebKit/Chromium engine or a phone runtime, and all of them do P-256 in
// hardware-accelerated code. It also keeps the pairing payload's
// fingerprint the only large thing on a QR code.
//
// Not a CA. Pinning compares the leaf's bytes, so nothing needs this
// certificate to sign anything, and a leaf that cannot sign is the
// smaller thing to hold if somebody ever takes the documented escape
// hatch and installs it into a trust store by hand.
func mint(now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("servercert: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("servercert: generate serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// The loopback names, and only those. They buy a pinning client
		// nothing — it compares bytes — but they cost nothing either, and
		// they keep a client that does check names from failing on the
		// one address every install can reach. A LAN address is
		// deliberately absent: it changes with the network, and a
		// certificate that had to be re-minted on a DHCP lease would
		// un-pin every paired device for moving rooms.
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("servercert: self-sign: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("servercert: parse the certificate just minted: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// encode renders the pair as the combined PEM file, certificate first so
// a human running `openssl x509 -in` on it gets the interesting half.
// The key half is what makes a failure here worth reporting rather than
// ignoring: a file holding a certificate and no key fails to load on the
// next boot, which is a re-mint nobody asked for.
func encode(cert tls.Certificate) ([]byte, error) {
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("servercert: marshal the private key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...), nil
}

// Fingerprint renders the pinning string for a DER-encoded certificate.
// One function, because the value the pairing payload carries and the
// value a client compares have to be the same spelling of the same
// digest.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return fingerprintPrefix + hex.EncodeToString(sum[:])
}
