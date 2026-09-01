package deviceclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDeviceKey_ReadNeverGenerates is the rule the browser half states and
// this half has to hold: a profile whose key went away while its session
// survived must answer "no key", not mint one. A fresh key names a
// thumbprint the stored session does not, so generating here would trade a
// clear answer for a refusal one round trip later under a reason
// describing a different problem.
func TestDeviceKey_ReadNeverGenerates(t *testing.T) {
	dir := t.TempDir()
	if _, err := DeviceKey(dir); !errors.Is(err, ErrNoDeviceKey) {
		t.Fatalf("DeviceKey on an empty profile = %v, want ErrNoDeviceKey", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the read created %v (err %v); it must create nothing", entries, err)
	}
}

// TestEnrollDeviceKey_PersistsOnceAndReturnsTheSameKey — re-pairing the
// same installation is the same device, and the backend adopts its row by
// thumbprint. A second key would strand the first row and leave this
// profile unable to present the sessions standing on it.
func TestEnrollDeviceKey_PersistsOnceAndReturnsTheSameKey(t *testing.T) {
	dir := t.TempDir()
	first, err := EnrollDeviceKey(dir)
	if err != nil {
		t.Fatalf("EnrollDeviceKey: %v", err)
	}
	second, err := EnrollDeviceKey(dir)
	if err != nil {
		t.Fatalf("EnrollDeviceKey again: %v", err)
	}
	if !first.Equal(second) {
		t.Fatal("a second enrolment minted a different key")
	}
	read, err := DeviceKey(dir)
	if err != nil {
		t.Fatalf("DeviceKey after enrolment: %v", err)
	}
	if !first.Equal(read) {
		t.Fatal("the persisted key is not the one enrolment returned")
	}

	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestDeviceKey_RefusesAnotherCurve — ES256 is the only algorithm the
// backend accepts and there is nothing to negotiate, so a P-384 key would
// sign proofs nothing can verify. Refusing at the read puts the failure
// where it can be diagnosed instead of inside a credential request.
func TestDeviceKey_RefusesAnotherCurve(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a P-384 key: %v", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, KeyFileName)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: keyPEMType, Bytes: encoded}), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := DeviceKey(dir); err == nil {
		t.Fatal("DeviceKey accepted a P-384 key, want an error")
	} else if errors.Is(err, ErrNoDeviceKey) {
		t.Fatalf("an unreadable key reported as an absent one: %v", err)
	}
}

// TestDeviceKey_RefusesAnEmptyProfileDir — an empty directory would put
// the device key in whatever the process's working directory happens to
// be, which is a key nobody can find again.
func TestDeviceKey_RefusesAnEmptyProfileDir(t *testing.T) {
	if _, err := DeviceKey(""); err == nil {
		t.Fatal("DeviceKey accepted an empty profile directory")
	}
	if _, err := EnrollDeviceKey(""); err == nil {
		t.Fatal("EnrollDeviceKey accepted an empty profile directory")
	}
}
