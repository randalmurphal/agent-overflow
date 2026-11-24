//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"
)

// A computer that limits pairing attempts answers the redemption before
// judging the link. The person adding it on the desktop is told to wait for
// the time it named, and nothing is saved.
func TestAddingAComputerThatLimitsPairingSaysToWait(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/pair" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: introducedBackendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "pairing"})
	if err != nil {
		t.Fatal(err)
	}
	manager, dir := newManager(t)
	_, err = manager.Add(context.Background(), deviceclient.LinkFragmentPrefix+base64.RawURLEncoding.EncodeToString(encoded))
	if err == nil {
		t.Fatal("adding a computer that limits pairing succeeded")
	}
	const want = "this computer is limiting pairing attempts; wait 30 seconds, then try again"
	if answer := transport.CallerError(err); answer.Message != want {
		t.Fatalf("the person reads %q (%v), want %q", answer.Message, err, want)
	}
	if _, loadErr := deviceclient.LoadSession(dir, introducedBackendID); loadErr == nil {
		t.Fatal("a refused pairing saved a session")
	}
}
