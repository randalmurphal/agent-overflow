package provideraccounts

import (
	"os"
	"testing"

	"agent-overflow/internal/testutil/providerpath"
)

// TestMain keeps every test from running an installed claude or codex
// found on PATH.
func TestMain(m *testing.M) {
	os.Exit(providerpath.Run(m))
}
