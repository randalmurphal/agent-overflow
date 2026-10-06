package frontendclient

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/nearby"
)

// TestLoopbackOnlyFrontendSendsNoMulticast: the end-to-end fixture's
// Connect to a computer screen asks for nearby computers on mount. Off the
// test network namespace that scan answers why nothing can be found and
// never reaches the multicast browser.
func TestLoopbackOnlyFrontendSendsNoMulticast(t *testing.T) {
	remotetest.Require(t)
	kerneltest.IsolateSpawns(t)
	previous := nearby.Interfaces
	t.Cleanup(func() { nearby.Interfaces = previous })
	var scans atomic.Int32
	nearby.Interfaces = func() ([]net.Interface, error) {
		scans.Add(1)
		return nil, nil
	}
	s, err := Serve(Config{Profiles: t.TempDir(), ConfigDir: t.TempDir(), ClientID: "fixture-client",
		Assets: fstest.MapFS{"index.html": {Data: []byte("frontend fixture")}}, Version: "test", LoopbackOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	if got, err := s.service.DiscoverComputers(context.Background()); !errors.Is(err, nearby.ErrIsolated) || len(got) != 0 {
		t.Fatalf("discovery = %+v, %v; want nearby.ErrIsolated", got, err)
	}
	if n := scans.Load(); n != 0 {
		t.Fatalf("a loopback-only frontend enumerated interfaces for %d multicast scans", n)
	}
}
