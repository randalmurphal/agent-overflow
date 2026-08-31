package harnessrpc

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// newHarnessPageMarker is an opaque per-backend value. It is placed in the
// page URL and returned only by the authenticated bootstrap endpoint.
func newHarnessPageMarker() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("harness page marker: generate nonce: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
