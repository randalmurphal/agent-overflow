//go:build !windows

package mcpapp

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/testutil/mockexec"
)

// The Antigravity store is decided only when `agy mcp list` runs: a cached
// status is answered without deciding it, even while deciding it fails.
func TestACachedAgyStatusDecidesNoStore(t *testing.T) {
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "agy"), "#!/bin/sh\necho 'demo  http  enabled  https://example.com/mcp'\n")
	var decided atomic.Int32
	var failing atomic.Bool
	service := New(Deps{
		Reaper:         procutiltest.Reaper(),
		ProviderBinary: func(string) string { return binary },
		AgyCanonicalHome: func(context.Context) (agy.Home, error) {
			decided.Add(1)
			if failing.Load() {
				return agy.Home{}, errors.New("the Keychain query failed")
			}
			return agy.CanonicalHome("", true)
		},
	})

	if _, err := service.GetMcpServerStatus("agy", "demo", false); err != nil {
		t.Fatalf("first status: %v", err)
	}
	if got := decided.Load(); got != 1 {
		t.Fatalf("the first status decided the store %d times, want once", got)
	}
	failing.Store(true)
	status, err := service.GetMcpServerStatus("agy", "demo", false)
	if err != nil || status.Name != "demo" {
		t.Fatalf("cached status = %+v, %v; want demo from the cache", status, err)
	}
	if got := decided.Load(); got != 1 {
		t.Fatalf("a cached status decided the store (%d decisions)", got)
	}
	if _, err := service.GetMcpServerStatus("agy", "demo", true); err == nil {
		t.Fatal("a forced status ran with a store that could not be decided")
	}
}
