package wsllauncher

import (
	"crypto/sha256"
	"encoding/hex"
)

// PayloadFingerprint identifies the exact embedded backend, including its SPA.
// Hashing a large embedded string must not allocate a second payload-sized copy.
func PayloadFingerprint(payload string) string {
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	for len(payload) > 0 {
		n := copy(buffer, payload)
		hash.Write(buffer[:n])
		payload = payload[n:]
	}
	return hex.EncodeToString(hash.Sum(nil))
}
