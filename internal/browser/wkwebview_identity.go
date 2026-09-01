package browser

import (
	"crypto/sha256"
	"fmt"
)

// The WKWebView engine's profile identity. Tag-free and free of cgo for the
// same reason webkitjs.go and webkitimage.go are: a malformed identifier is a
// silent failure — WebKit rejects it, the workspace quietly falls back to an
// in-memory store, and two workspaces could end up sharing one — so the rule
// that produces it is compiled and tested on every platform.

// wkStoreIdentifier turns a workspace root into the stable UUID
// +dataStoreForIdentifier: keys its directory on. The workspace DIGEST, not its
// path: a workspace root can be long, holds characters an identifier cannot,
// and must not be readable from anything WebKit writes. The RFC 4122 version
// and variant bits are set because WebKit takes an NSUUID, and the all-zero
// UUID is documented as invalid.
func wkStoreIdentifier(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	var raw [16]byte
	copy(raw[:], digest[:16])
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
