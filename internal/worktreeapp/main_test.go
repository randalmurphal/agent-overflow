package worktreeapp

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
)

// TestMain keeps every test from running an installed claude or codex
// found on PATH.
func TestMain(m *testing.M) {
	os.Exit(providerpath.Guard(func() int { return storetest.Run(m) }))
}
