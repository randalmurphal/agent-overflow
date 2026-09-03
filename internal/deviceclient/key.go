package deviceclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
)

// KeyFileName is the device key under the profile directory. PKCS#8 PEM,
// written at 0600 by internal/atomicfile, which is also what keeps a
// half-written key from ever being read back.
const KeyFileName = "device-key.pem"

// keyPEMType is the block label. The standard one for PKCS#8, so
// `openssl pkey -in device-key.pem -text` works on it unaided.
const keyPEMType = "PRIVATE KEY"

// ErrNoDeviceKey means this profile holds no device key. Distinct from an
// unreadable one: nothing has enrolled here yet, which is a state and not
// a fault.
var ErrNoDeviceKey = errors.New("deviceclient: this profile holds no device key")

// DeviceKey reads the profile's key. It NEVER generates one.
//
// The rule is `frontend/src/lib/transport/deviceKey.ts`'s, and it is here
// for the same reason: generating on a miss is the wrong answer for the
// case that actually happens, which is a profile whose key file went away
// while its session file survived. A fresh key is not a recovery there —
// the stored session names the OLD thumbprint, so every request would be
// refused anyway, one round trip later and under a reason
// (`key_mismatch`) that describes a different problem. Answering
// ErrNoDeviceKey lets the caller forget the session and ask the person to
// pair, which is the only thing that works.
//
// Generation belongs to enrollment alone, which is the one moment a device
// is allowed to decide which key names it: EnrollDeviceKey.
func DeviceKey(dir string) (*ecdsa.PrivateKey, error) {
	path, err := keyPath(dir)
	if err != nil {
		return nil, err
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoDeviceKey
		}
		return nil, fmt.Errorf("deviceclient: read %s: %w", path, err)
	}
	return decodeKey(stored, path)
}

// EnrollDeviceKey returns the profile's key, minting and persisting one
// when it holds none. Only pairing calls this.
//
// An EXISTING key is returned rather than replaced: re-pairing the same
// installation is the same device, and the backend adopts its row by
// thumbprint (`internal/identity/AGENTS.md` § Pairing). Minting a second
// key would strand the first row and leave this profile unable to present
// the sessions still standing on it.
//
// The key is persisted BEFORE it is returned, so a key that signs a
// redemption and then fails to store cannot enroll a device this process
// can never present again.
func EnrollDeviceKey(dir string) (*ecdsa.PrivateKey, error) {
	held, err := DeviceKey(dir)
	if err == nil {
		return held, nil
	}
	if !errors.Is(err, ErrNoDeviceKey) {
		return nil, err
	}
	path, err := keyPath(dir)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("deviceclient: generate the device key: %w", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("deviceclient: encode the device key: %w", err)
	}
	if err := atomicfile.Write(path, pem.EncodeToMemory(&pem.Block{Type: keyPEMType, Bytes: encoded})); err != nil {
		return nil, fmt.Errorf("deviceclient: persist the device key: %w", err)
	}
	return key, nil
}

// keyPath resolves the key file, refusing an empty profile directory
// rather than writing into the process's working directory.
func keyPath(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("deviceclient: no profile directory to keep the device key in")
	}
	return filepath.Join(dir, KeyFileName), nil
}

// decodeKey parses stored PEM into the one key type this client presents.
//
// A key on another curve is refused rather than carried: ES256 is the only
// algorithm the backend accepts and there is no agility to negotiate, so a
// P-384 key here would sign proofs nothing can verify and the failure
// would land inside a credential request instead of at the read.
func decodeKey(stored []byte, path string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(stored)
	if block == nil || block.Type != keyPEMType {
		return nil, fmt.Errorf("deviceclient: %s is not a %s PEM block", path, keyPEMType)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("deviceclient: parse %s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("deviceclient: %s is not an ECDSA P-256 key", path)
	}
	return key, nil
}
