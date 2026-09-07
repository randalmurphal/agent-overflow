package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/servercert"
)

func TestCustomNetworkDialSurvivesPairOpenAndRouteRepairWithoutChangingTLS(t *testing.T) {
	be := &backend{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if r.Header.Get(SessionCredentialHeader) != "" {
				t.Error("health carried credentials")
			}
			json.NewEncoder(w).Encode(map[string]string{"backendId": "backend-a"})
			return
		}
		be.route(w, r)
	}))
	defer server.Close()
	var mu sync.Mutex
	dialed := make(map[string]bool)
	option := WithDialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed[address] = true
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	dir := t.TempDir()
	link := Link{BackendID: "backend-a", Endpoint: "https://first.invalid", CertFingerprint: servercert.Fingerprint(server.Certificate().Raw), Token: "test-token"}
	paired, _, err := Pair(context.Background(), dir, link, "test", "test", option)
	if err != nil {
		t.Fatal(err)
	}
	paired.http.CloseIdleConnections()
	reopened, err := Open(dir, paired.Session(), option)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.http.CloseIdleConnections()
	if err := routeRequest(reopened, http.MethodPost, "/auth/ticket"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.RepairAddress(context.Background(), "https://second.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := routeRequest(reopened, http.MethodPost, "/auth/ticket"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, address := range []string{"first.invalid:443", "second.invalid:443"} {
		if !dialed[address] {
			t.Errorf("did not dial %s", address)
		}
	}
	mu.Unlock()
	wrong := NewPinnedTransport("sha256:"+strings.Repeat("00", 32), option)
	defer wrong.CloseIdleConnections()
	client := http.Client{Transport: wrong}
	response, err := client.Get("https://third.invalid")
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrCertificateMismatch) {
		t.Fatalf("custom dial bypassed pin: %v", err)
	}
}
