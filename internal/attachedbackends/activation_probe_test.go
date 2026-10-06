package attachedbackends

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
)

// TestSetActivationProbePacesAwait: the probe a boot sets on the manager
// reaches the clients it builds, so Await asks the far machine on that
// interval rather than deviceclient's.
func TestSetActivationProbePacesAwait(t *testing.T) {
	remotetest.Require(t)
	var renewals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/token") {
			renewals.Add(1)
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	manager, dir := newManager(t)
	manager.SetActivationProbe(20 * time.Millisecond)
	legacy := false
	seed(t, dir, deviceclient.Session{
		RefreshRecovery: &legacy, BackendID: "aaa", Endpoint: server.URL, SessionID: "s1", Credential: "c1",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(), RefreshSecret: "r1",
		RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Await(ctx, "aaa"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await through an outage = %v, want the deadline", err)
	}
	if got := renewals.Load(); got < 5 {
		t.Fatalf("asked %d times in a second at a 20ms probe, want at least 5", got)
	}
}
