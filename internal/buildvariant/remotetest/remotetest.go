// Package remotetest lets a test that exercises remote access skip itself
// in a build compiled without it (internal/buildvariant), where the
// behavior it checks does not exist. The noremote counterparts of those
// tests assert the refusal instead.
package remotetest

import (
	"testing"

	"agent-overflow/internal/buildvariant"
)

// Require skips t in a build without remote access.
func Require(t testing.TB) {
	t.Helper()
	if !buildvariant.RemoteAccess {
		t.Skip("exercises remote access, which this build does not have")
	}
}
