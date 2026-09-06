package deviceclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/servercert"
)

func TestRepairAddressPreservesPairingAndVerifiesBeforeCredentials(t *testing.T) {
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("retired address was used") }))
	client, dir := openAgainst(t, &backend{Server: first}, func(session *Session) {
		session.CertFingerprint = servercert.Fingerprint(first.Certificate().Raw)
		session.PendingNextSecret = "pending-refresh-successor"
	})
	first.Close()
	before := client.Session()
	var calls atomic.Int32
	_, target := candidateServer(t, before.BackendID, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(SessionCredentialHeader) != before.Credential {
			t.Error("pairing credential changed")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// httptest's test certificate is the same on both listeners. Repair must
	// reuse that original pin, not acquire a pin from this new address.
	if target.CertFingerprint != before.CertFingerprint {
		t.Fatal("fixture listeners need the same pinned certificate")
	}
	verified, err := client.RepairAddress(context.Background(), target.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if verified != target || calls.Load() != 0 {
		t.Fatal("repair sent credentials or changed trust")
	}
	after, err := LoadSession(dir, before.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionID != before.SessionID || after.Endpoint != before.Endpoint || after.Credential != before.Credential || after.PendingNextSecret != before.PendingNextSecret {
		t.Fatal("address repair changed pairing or pending renewal")
	}
	if err := routeRequest(client, http.MethodGet, "/next"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("reconnect did not use the verified address")
	}
}

func TestRepairAddressRejectsAnotherBackendWithoutSavingIt(t *testing.T) {
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(first.Close)
	client, dir := openAgainst(t, &backend{Server: first}, func(session *Session) {
		session.CertFingerprint = servercert.Fingerprint(first.Certificate().Raw)
	})
	_, target := candidateServer(t, "different-backend", func(w http.ResponseWriter, r *http.Request) { t.Error("credential reached a different backend") })
	if _, err := client.RepairAddress(context.Background(), target.Endpoint); err == nil {
		t.Fatal("accepted a different backend")
	}
	held, err := LoadSession(dir, client.Session().BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Routes) != 0 || held.LastEndpoint != "" {
		t.Fatal("failed verification changed the saved address")
	}
}

func TestRepairAddressDoesNotRestoreTrustReplacedWhileChecking(t *testing.T) {
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(first.Close)
	client, dir := openAgainst(t, &backend{Server: first}, func(session *Session) {
		session.CertFingerprint = servercert.Fingerprint(first.Certificate().Raw)
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"backendId": client.Session().BackendID})
	}))
	t.Cleanup(target.Close)
	t.Cleanup(unblock)
	result := make(chan error, 1)
	go func() { _, err := client.RepairAddress(context.Background(), target.URL); result <- err }()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("health probe did not start: %v", err)
	}
	newPin := "sha256:" + strings.Repeat("b", 64)
	learnRoutes(t, client, computerroute.Route{Endpoint: client.Endpoint(), CertFingerprint: newPin})
	unblock()
	if err := <-result; err == nil {
		t.Fatal("repair restored superseded certificate trust")
	}
	held, err := LoadSession(dir, client.Session().BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Routes) != 1 || held.Routes[0].CertFingerprint != newPin {
		t.Fatal("repair replaced the newer trust decision")
	}
}
