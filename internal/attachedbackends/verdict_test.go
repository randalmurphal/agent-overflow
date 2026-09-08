package attachedbackends

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/transport"
)

// peer stands in for one attached backend's credential routes and manifest.
// It honours only the credential it most recently issued, and a rotation
// either issues the next one or refuses with a reason — the two answers a
// real backend gives an aged credential and a revoked session.
type peer struct {
	*httptest.Server
	mu         sync.Mutex
	credential string
	refusal    string
	rotations  atomic.Int32
}

// newPeer honours nothing until its first rotation: the credential a test
// seeds is one that has aged out.
func newPeer(t *testing.T) *peer {
	t.Helper()
	p := &peer{}
	p.Server = httptest.NewServer(http.HandlerFunc(p.route))
	t.Cleanup(p.Close)
	return p
}

func (p *peer) honours(r *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credential != "" && r.Header.Get(deviceclient.SessionCredentialHeader) == p.credential
}

// revoke ends the session: nothing is honoured and every rotation refuses.
func (p *peer) revoke() {
	p.mu.Lock()
	p.credential, p.refusal = "", "revoked_session"
	p.mu.Unlock()
}

func (p *peer) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/bootstrap.json":
		if !p.honours(r) {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"backendId": "peer", "backendName": "Peer"})
	case "/auth/ticket":
		if !p.honours(r) {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket"})
	case "/auth/token":
		issued := int(p.rotations.Add(1))
		p.mu.Lock()
		refusal := p.refusal
		if refusal == "" {
			p.credential = "credential-" + strconv.Itoa(issued)
		}
		credential := p.credential
		p.mu.Unlock()
		if refusal != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": refusal})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId":          "session",
			"credential":         credential,
			"expiresAtMs":        time.Now().Add(time.Hour).UnixMilli(),
			"refreshSecret":      "refresh-" + strconv.Itoa(issued),
			"refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
		})
	default:
		http.NotFound(w, r)
	}
}

// seedPeer stores a live-looking session on p holding credential-0, which
// p no longer honours.
func seedPeer(t *testing.T, dir string, p *peer, own bool) {
	t.Helper()
	legacy := false
	seed(t, dir, deviceclient.Session{
		OwnDevice: own, RefreshRecovery: &legacy,
		BackendID: "peer", Endpoint: p.URL, SessionID: "session", Credential: "credential-0",
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		RefreshSecret: "refresh-0", RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli(),
	})
}

// TestManifestRotatesAnAgedCredentialAndRetiresARevokedSession — a refused
// manifest is ambiguous, and the rotation is what tells an aged credential
// from a session the far side ended: the first serves after one renewal,
// the second is a typed verdict that evicts the carrier and tells the
// observer, instead of the 503 every outage answers.
func TestManifestRotatesAnAgedCredentialAndRetiresARevokedSession(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	seedPeer(t, dir, p, false)
	var ended []string
	manager.SetSessionEnded(func(id string) { ended = append(ended, id) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	held, err := manager.carrier("peer")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := held.Manifest(ctx)
	if err != nil {
		t.Fatalf("manifest with an aged credential: %v", err)
	}
	if manifest.BackendID != "peer" || p.rotations.Load() != 1 {
		t.Fatalf("manifest = %+v after %d rotations, want the peer's after one", manifest, p.rotations.Load())
	}
	if stored, err := deviceclient.LoadSession(dir, "peer"); err != nil || stored.Credential != "credential-1" {
		t.Fatalf("stored credential = %q (%v), want the rotated one", stored.Credential, err)
	}
	if len(ended) != 0 {
		t.Fatalf("an aged credential ended the session: %v", ended)
	}

	p.revoke()
	_, err = held.Manifest(ctx)
	if !errors.Is(err, transport.ErrAttachedSessionEnded) || !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("manifest after revocation = %v, want the typed verdict", err)
	}
	if len(ended) != 1 || ended[0] != "peer" {
		t.Errorf("observer told %v, want the one ended pairing", ended)
	}
	if manager.Carrier("peer") != nil {
		t.Error("a revoked pairing still has a carrier")
	}
	if _, err := deviceclient.LoadSession(dir, "peer"); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Errorf("profile after revocation: %v, want it gone", err)
	}
	if profiles := manager.Attached(); len(profiles) != 0 {
		t.Errorf("attached after revocation = %+v, want none", profiles)
	}
}

// TestCarrierReopensAProfileWhoseCachedOwnerRetired — a retired owner never
// authorizes again, so a cache holding one is a miss: the file on disk is
// read again, and only its absence answers nil.
func TestCarrierReopensAProfileWhoseCachedOwnerRetired(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1",
	})
	first, err := manager.carrier("aaa")
	if err != nil {
		t.Fatal(err)
	}
	first.client.Retire()
	second, err := manager.carrier("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second.client.Retired() {
		t.Fatal("a retired owner was answered from the cache")
	}
	if second.client.Session().Credential != "c1" {
		t.Errorf("reopened credential = %q, want the stored one", second.client.Session().Credential)
	}
	second.client.Retire()
	if err := deviceclient.ForgetSession(dir, "aaa"); err != nil {
		t.Fatal(err)
	}
	if manager.Carrier("aaa") != nil {
		t.Error("a retired owner with no file behind it still answers as a carrier")
	}
}

// TestRemoveWithNoLiveCarrierForgetsTheFile — a profile nothing has opened
// this launch has no owner to retire, and is removed from disk directly.
func TestRemoveWithNoLiveCarrierForgetsTheFile(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1",
	})
	if err := manager.Remove("aaa"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := deviceclient.LoadSession(dir, "aaa"); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Errorf("profile after removal: %v, want it gone", err)
	}
	if profiles := manager.Attached(); len(profiles) != 0 {
		t.Errorf("attached after removal = %+v, want none", profiles)
	}
}
