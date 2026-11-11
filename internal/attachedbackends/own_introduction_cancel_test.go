//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/servercert"
)

// An accepted introduction spends the far side's single-use invitation. A
// caller that stops waiting once the redemption was sent still gets the
// pairing saved, activated and published.
func TestOwnIntroductionFinishesWhenTheCallerCancelsDuringRedemption(t *testing.T) {
	t.Parallel()
	const backendID = "backend-far"
	arrived := make(chan struct{})
	release := make(chan struct{})
	var rotations atomic.Int32
	grant := func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "session-1", "credential": "ao1.issued", "expiresAtMs": time.Now().Add(time.Hour).UnixMilli(),
			"refreshSecret": "refresh-" + time.Now().String(), "refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
			"scopes": []string{"threads:read"},
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"backendId": backendID})
		case "/auth/pair":
			close(arrived)
			<-release
			grant(w)
		case "/auth/token":
			rotations.Add(1)
			grant(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	m, err := New(t.TempDir(), "this computer", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	var published atomic.Int32
	m.SetChanged(func(SetChange) { published.Add(1) })

	route := computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)}
	route, err = computerroute.Normalize(route)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{
		Version: deviceclient.LinkVersion, Purpose: "own-introduction", BackendID: backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "invitation",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(encoded)

	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		added bool
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		added, err := m.AcceptOwnDeviceIntroduction(ctx, raw, []computerroute.Route{route})
		done <- answer{added, err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the redemption never arrived")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	var got answer
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("AcceptOwnDeviceIntroduction did not return")
	}
	if got.err != nil || !got.added {
		t.Fatalf("introduction after the caller cancelled = added %t, %v; want it enrolled", got.added, got.err)
	}
	if saved, err := deviceclient.LoadSession(m.dir, backendID); err != nil || !saved.OwnDevice {
		t.Fatalf("saved session = %+v, %v; want the own-device pairing", saved, err)
	}
	if rotations.Load() == 0 {
		t.Fatal("the pairing was never activated")
	}
	if published.Load() == 0 {
		t.Fatal("the new pairing was not published")
	}
}
