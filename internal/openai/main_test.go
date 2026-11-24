package openai

import (
	"os"
	"testing"

	"agent-overflow/internal/testutil/providerpath"
)

// TestMain keeps every test from running an installed provider CLI found on
// PATH: this package links internal/provider through the settings tables.
func TestMain(m *testing.M) {
	os.Exit(providerpath.Run(m))
}
