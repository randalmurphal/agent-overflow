package servercert

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// A reload has to answer the SAME fingerprint. It is what paired devices
// pinned, so a restart that changed it would silently lock every one of
// them out.
func TestLoadReloadsTheSameCertificateItMinted(t *testing.T) {
	dir := t.TempDir()

	first, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !first.Minted {
		t.Fatal("the first Load in an empty directory did not report minting")
	}
	if first.Replaced != "" {
		t.Fatalf("first Load replaced something: %q", first.Replaced)
	}

	second, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if second.Minted {
		t.Fatal("the second Load minted again instead of loading what the first wrote")
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("fingerprint moved across a reload: %q then %q", first.Fingerprint, second.Fingerprint)
	}
	if second.Certificate.Leaf == nil {
		t.Fatal("a loaded certificate carries no parsed leaf")
	}
	if len(second.Certificate.Certificate) != 1 {
		t.Fatalf("loaded chain length = %d, want the single self-signed leaf", len(second.Certificate.Certificate))
	}
}

// The fingerprint is a wire format: the pairing payload carries this
// exact spelling and a pinning client compares against it, so both the
// shape and the bytes it digests are pinned here.
func TestFingerprintIsSHA256OfTheLeafInTheDeclaredSpelling(t *testing.T) {
	material, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(material.Fingerprint) {
		t.Fatalf("fingerprint %q is not sha256:<lowercase hex>", material.Fingerprint)
	}
	sum := sha256.Sum256(material.Certificate.Certificate[0])
	if want := "sha256:" + hex.EncodeToString(sum[:]); material.Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q (the digest of the leaf DER)", material.Fingerprint, want)
	}
	if got := Fingerprint(material.Certificate.Certificate[0]); got != material.Fingerprint {
		t.Fatalf("Fingerprint(leaf) = %q, but Load answered %q", got, material.Fingerprint)
	}
}

// Material this package cannot read is replaced rather than made fatal —
// and the replacement is announced, because it un-pins every paired
// device.
func TestLoadRemintsOverMaterialItCannotRead(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("-----BEGIN CERTIFICATE-----\ntruncated"), 0o600); err != nil {
		t.Fatalf("damage the stored material: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("Load over damaged material: %v", err)
	}
	if !second.Minted {
		t.Fatal("damaged material loaded as if it were usable")
	}
	if second.Replaced == "" {
		t.Fatal("a re-mint over existing material reported no reason, so nothing can surface it")
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("the replacement carries the fingerprint of the certificate it replaced")
	}

	third, err := Load(dir)
	if err != nil {
		t.Fatalf("third Load: %v", err)
	}
	if third.Minted || third.Fingerprint != second.Fingerprint {
		t.Fatalf("the replacement did not persist: minted=%v fingerprint %q vs %q",
			third.Minted, third.Fingerprint, second.Fingerprint)
	}
}

// An expired certificate is the one date this package acts on. A clock
// that reads EARLIER than the mint is deliberately not: see
// usableMaterial.
func TestLoadRemintsAnExpiredCertificateAndKeepsAnEarlyClock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	expired, err := mint(time.Now().Add(-2 * Validity))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	encoded, err := encode(expired)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write expired material: %v", err)
	}
	replaced, err := Load(dir)
	if err != nil {
		t.Fatalf("Load over an expired certificate: %v", err)
	}
	if !replaced.Minted || replaced.Replaced == "" {
		t.Fatalf("an expired certificate was served as-is: minted=%v replaced=%q", replaced.Minted, replaced.Replaced)
	}

	// The same material read by a machine whose clock is a year behind
	// the mint: NotBefore has not arrived, and that must not cost the
	// user every pairing they hold.
	fresh, err := mint(time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, why := usableMaterial(mustEncode(t, fresh), time.Now().Add(-365*24*time.Hour)); why != nil {
		t.Fatalf("a clock reading before the mint discarded usable material: %v", why)
	}
}

// The dates and names a minted certificate carries, in one place: the
// long life is the package's central design call, and the loopback SANs
// are what keep a name-checking client working on the one address every
// install has.
func TestMintedCertificateIsLongLivedAndNamesLoopback(t *testing.T) {
	material, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	leaf := material.Certificate.Leaf
	if remaining := time.Until(leaf.NotAfter); remaining < Validity-2*clockSkew {
		t.Fatalf("certificate expires in %s, want about %s", remaining, Validity)
	}
	if !leaf.NotBefore.Before(time.Now()) {
		t.Fatalf("NotBefore %s is not backdated for clock skew", leaf.NotBefore)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Fatalf("DNS names = %v, want [localhost]", leaf.DNSNames)
	}
	for _, want := range []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback} {
		found := false
		for _, got := range leaf.IPAddresses {
			if got.Equal(want) {
				found = true
			}
		}
		if !found {
			t.Errorf("certificate does not name %s; IP SANs are %v", want, leaf.IPAddresses)
		}
	}
	// Not a CA: pinning compares the leaf's bytes, so nothing needs this
	// certificate to sign anything.
	if leaf.IsCA {
		t.Error("the certificate claims to be a CA")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("the certificate may sign other certificates")
	}
}

// The file holds a private key, so it is owner-only. Checked on the
// platforms whose mode bits mean that.
func TestStoredMaterialIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not describe Windows ACLs")
	}
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("stored certificate mode = %04o, want 0600", mode)
	}
}

// A directory nobody named is a mistake at the call site, not a reason
// to mint something that cannot survive a restart.
func TestLoadRefusesAnUnnamedDirectory(t *testing.T) {
	if _, err := Load("  "); err == nil {
		t.Fatal("Load minted a certificate with nowhere to keep it")
	}
}

func mustEncode(t *testing.T, cert tls.Certificate) []byte {
	t.Helper()
	encoded, err := encode(cert)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}
