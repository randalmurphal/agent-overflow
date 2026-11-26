package trackerdash

import (
	"os"
	"testing"

	"agent-overflow/internal/testutil/providerpath"
)

// TestMain keeps every test from running an installed claude or codex
// found on PATH: the package links a provider spawn package through
// internal/store.
func TestMain(m *testing.M) {
	os.Exit(providerpath.Run(m))
}
