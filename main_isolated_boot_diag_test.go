package main

import (
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/observability/pprofserve"
)

// TestIsolatedPprofEphemeralAddr: pprofserve.DefaultAddr is a fixed
// singleton (127.0.0.1:6363) and isolated boots are the one shape run
// N-at-a-time, so a bare enable must not claim it.
func TestIsolatedPprofEphemeralAddr(t *testing.T) {
	for _, raw := range []string{"1", "true", "TRUE", " 1 "} {
		if got := isolatedPprofEphemeralAddr(raw); got != "127.0.0.1:0" {
			t.Errorf("isolatedPprofEphemeralAddr(%q) = %q, want an ephemeral loopback port", raw, got)
		}
	}
	// An explicit address is the operator choosing a port; honour it.
	for _, raw := range []string{"127.0.0.1:7777", pprofserve.DefaultAddr, "", "0", "false"} {
		if got := isolatedPprofEphemeralAddr(raw); got != "" {
			t.Errorf("isolatedPprofEphemeralAddr(%q) = %q, want it left alone", raw, got)
		}
	}
}

// TestUnpinIsolatedPprofPortRewritesTheEnvironment covers the seam
// itself: pprofserve reads the variable, so the rewrite has to land in
// the environment before StartIfEnabled runs.
func TestUnpinIsolatedPprofPortRewritesTheEnvironment(t *testing.T) {
	t.Setenv(pprofserve.EnvVar, "1")
	unpinIsolatedPprofPort()
	if got := os.Getenv(pprofserve.EnvVar); got != "127.0.0.1:0" {
		t.Fatalf("%s = %q after unpin, want 127.0.0.1:0", pprofserve.EnvVar, got)
	}
	// And the listener actually comes up on a port that is not the
	// singleton — two isolated boots must be able to coexist.
	addr, stop, err := pprofserve.StartIfEnabled()
	if err != nil {
		t.Fatalf("StartIfEnabled: %v", err)
	}
	defer stop()
	if addr == "" || strings.HasSuffix(addr, ":6363") {
		t.Fatalf("bound %q, want an ephemeral loopback port", addr)
	}

	t.Setenv(pprofserve.EnvVar, "127.0.0.1:7777")
	unpinIsolatedPprofPort()
	if got := os.Getenv(pprofserve.EnvVar); got != "127.0.0.1:7777" {
		t.Fatalf("explicit address was rewritten to %q", got)
	}
}

// TestIsolatedDevAssetWarning: --harness/--soak honor
// FRONTEND_DEVSERVER_URL, and that variable is EXPORTED by `make dev` —
// so a harness launched from that terminal silently measures the dev
// bundle. The warning is the only thing that says so.
func TestIsolatedDevAssetWarning(t *testing.T) {
	if got := isolatedDevAssetWarning(""); got != "" {
		t.Fatalf("warned with no dev server: %q", got)
	}
	got := isolatedDevAssetWarning("http://localhost:5173")
	if !strings.Contains(got, "WARNING") {
		t.Errorf("warning is not loud: %q", got)
	}
	for _, want := range []string{"http://localhost:5173", "FRONTEND_DEVSERVER_URL", "DEV"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q omits %q", got, want)
		}
	}
}
