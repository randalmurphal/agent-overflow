package app

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// The paired-device client module restates five spellings this side owns:
// the pairing fragment prefix a minted URL carries, the three credential
// routes, and the two header names. None of them can be imported across
// the language boundary, so this gate reads the TypeScript source and
// fails on drift — the same arrangement reason_gate_test.go keeps for the
// refusal vocabulary, kept here because this package mints the URL whose
// fragment that module parses. A mismatch is a pairing link that opens
// the app and silently does nothing.
func TestDeviceSessionModuleMatchesTheWire(t *testing.T) {
	// This suite runs from the repository root (main_test.go's TestMain).
	const modulePath = "frontend/src/lib/transport/deviceSession.ts"
	source, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read %s: %v", modulePath, err)
	}
	module := string(source)

	for name, spelling := range map[string]string{
		"pairing fragment prefix":   fmt.Sprintf("'%s'", pairingFragmentPrefix),
		"pairing route":             fmt.Sprintf("'%s'", transport.AuthPairPath),
		"token route":               fmt.Sprintf("'%s'", transport.AuthTokenPath),
		"ticket route":              fmt.Sprintf("'%s'", transport.AuthTicketPath),
		"session credential header": fmt.Sprintf("'%s'", transport.SessionCredentialHeader),
		"device key header":         fmt.Sprintf("'%s'", transport.DeviceKeyHeader),
	} {
		if !strings.Contains(module, spelling) {
			t.Errorf("%s does not spell the %s %s", modulePath, name, spelling)
		}
	}
}
