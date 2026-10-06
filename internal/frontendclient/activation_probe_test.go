package frontendclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/kerneltest"
)

// TestServeHandsTheActivationProbeToItsComputers: Config.ActivationProbe
// paces a pending pairing's confirmation poll, so the end-to-end fixture
// asks on the shortened interval rather than the product one.
func TestServeHandsTheActivationProbeToItsComputers(t *testing.T) {
	remotetest.Require(t)
	kerneltest.IsolateSpawns(t)
	var renewals atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/token") {
			renewals.Add(1)
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	profiles := t.TempDir()
	if _, err := deviceclient.EnrollDeviceKey(profiles); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-4111-8111-111111111111"
	legacy := false
	if err := deviceclient.SaveSession(profiles, deviceclient.Session{
		RefreshRecovery: &legacy, BackendID: id, Endpoint: remote.URL, SessionID: "s1", Credential: "c1",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(), RefreshSecret: "r1",
		RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s, err := Serve(Config{Profiles: profiles, ConfigDir: t.TempDir(), ClientID: "fixture-client",
		Assets: fstest.MapFS{"index.html": {Data: []byte("frontend fixture")}}, Version: "test",
		ActivationProbe: 20 * time.Millisecond})
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.service.computers.Await(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await through an outage = %v, want the deadline", err)
	}
	if got := renewals.Load(); got < 5 {
		t.Fatalf("asked %d times in a second at a 20ms probe, want at least 5", got)
	}
}
