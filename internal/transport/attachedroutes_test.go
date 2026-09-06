package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAttached is one installation's attached set, recorded rather than
// carried: nothing here reaches a network, and the point of every test
// below is which requests arrive at the seam at all.
type fakeAttached struct {
	profiles []AttachedProfile
	carriers map[string]*fakeCarrier
}

func (f *fakeAttached) Attached() []AttachedProfile { return f.profiles }

func (f *fakeAttached) Carrier(id string) BackendCarrier {
	carrier, ok := f.carriers[id]
	if !ok {
		return nil
	}
	return carrier
}

type fakeCarrier struct {
	manifest    AttachedManifest
	manifestErr error

	upgrades  atomic.Int64
	transfers atomic.Int64
	// lastPath is what the byte relay was asked to carry, so a test can
	// see that the per-backend prefix reached the carrier intact.
	lastPath atomic.Value
}

func (c *fakeCarrier) Manifest(context.Context) (AttachedManifest, error) {
	if c.manifestErr != nil {
		return AttachedManifest{}, c.manifestErr
	}
	return c.manifest, nil
}

func (c *fakeCarrier) CarryUpgrade(w http.ResponseWriter, _ *http.Request) {
	c.upgrades.Add(1)
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func (c *fakeCarrier) CarryTransfer(w http.ResponseWriter, r *http.Request) {
	c.transfers.Add(1)
	c.lastPath.Store(r.URL.Path)
	w.WriteHeader(http.StatusOK)
}

// newAttachedFixture starts a server with one attached backend, "mini".
func newAttachedFixture(t *testing.T) (*serverFixture, *fakeCarrier) {
	t.Helper()
	carrier := &fakeCarrier{manifest: AttachedManifest{
		BackendID:         "store-uuid-mini",
		ReplicaGeneration: "gen-7",
		BackendName:       "workshop-mini",
		LaunchID:          "launch-mini",
		SessionScopes:     []string{"files:read", "git:operate"},
	}}
	set := &fakeAttached{
		profiles: []AttachedProfile{{ID: "mini", BackendID: "store-uuid-mini", Name: "The Mini", Nickname: "The Mini"}},
		carriers: map[string]*fakeCarrier{"mini": carrier},
	}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AttachedBackends = set })
	return f, carrier
}

// attachedRequest builds a request carrying the page credential the way a
// local caller that is not a browser does.
func attachedRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", req.URL, err)
	}
	return resp
}

// TestAttachedRoutesAreAbsentWithoutAnAttachedSet — a backend that
// attaches to nothing must not answer these paths at all, so the SPA
// bundle's own file server keeps them and a probe learns nothing.
func TestAttachedRoutesAreAbsentWithoutAnAttachedSet(t *testing.T) {
	f := newServerFixture(t)
	for _, path := range []string{"/ws/backend/mini", "/bootstrap/mini.json", "/backend/mini/attachments/x"} {
		resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+path))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d with no attached set, want 404", path, resp.StatusCode)
		}
	}
}

// TestAttachedBootstrapCarriesTheFarSidesIdentity — the whole reason this
// route exists rather than the page reading the far side's manifest
// itself: the identity crosses, and the wsUrl is rewritten to name THIS
// listener, which is the only wsUrl the SPA accepts.
func TestAttachedBootstrapCarriesTheFarSidesIdentity(t *testing.T) {
	f, _ := newAttachedFixture(t)
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap/mini.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.SessionScopes) != 2 || got.SessionScopes[1] != "git:operate" {
		t.Fatalf("carried grants missing: %v", got.SessionScopes)
	}
	if got.BackendID != "store-uuid-mini" || got.ReplicaGeneration != "gen-7" {
		t.Errorf("store identity = %q/%q, want the far side's", got.BackendID, got.ReplicaGeneration)
	}
	if got.BackendName != "workshop-mini" || got.LaunchID != "launch-mini" {
		t.Errorf("name/launch = %q/%q, want the far side's", got.BackendName, got.LaunchID)
	}
	if !got.Remote {
		t.Error("remote = false; a machine reached through this route is never the page's own")
	}
	want := "ws://" + f.srv.Addr() + "/ws/backend/mini"
	if got.WSURL != want {
		t.Errorf("wsUrl = %q, want this listener's own %q", got.WSURL, want)
	}
	// The far side's own listener facts must not answer for this page.
	if got.Harness || got.PageMarker != "" || got.PasskeysAvailable {
		t.Errorf("a listener fact crossed the hop: %+v", got)
	}
}

// TestAttachedBootstrapReportsAnUnreachableMachineAsTransient — a sleeping
// laptop must not read as one that has forgotten this device: the SPA's
// terminal state is reserved for a verdict, and a 404 here is a verdict.
func TestAttachedBootstrapReportsAnUnreachableMachineAsTransient(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	carrier.manifestErr = errors.New("dial: no route to host")
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap/mini.json"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestAttachedRoutesRefuseAnUnknownBackend — a page whose manifest is a
// moment stale asks for a backend that has just been removed, and gets
// the same unfingerprintable 404 a bad credential does.
func TestAttachedRoutesRefuseAnUnknownBackend(t *testing.T) {
	f, _ := newAttachedFixture(t)
	for _, path := range []string{"/ws/backend/gone", "/bootstrap/gone.json", "/backend/gone/attachments/x"} {
		resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+path))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d for an unattached backend, want 404", path, resp.StatusCode)
		}
	}
}

// TestAttachedRoutesRefuseAWrongCredential — the page credential is the
// whole admission. Without it a local process that is not this page could
// use this listener to reach a machine it never paired with.
func TestAttachedRoutesRefuseAWrongCredential(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	for _, path := range []string{"/ws/backend/mini", "/bootstrap/mini.json", "/backend/mini/attachments/x"} {
		req := attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+path)
		req.Header.Set("Authorization", "Bearer not-the-token")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d for a wrong credential, want 404", path, resp.StatusCode)
		}
	}
	if carrier.upgrades.Load() != 0 || carrier.transfers.Load() != 0 {
		t.Fatal("a refused request still reached the carrier")
	}
}

// TestAttachedTransferCarriesOnlyTheAttachmentSubtree — the subtree
// exists for the far side's attachment routes and for nothing else, so a
// path naming something else is a 404 rather than a hop through this
// machine's pinned credential.
func TestAttachedTransferCarriesOnlyTheAttachmentSubtree(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/backend/mini/healthz"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a non-attachment path answered %d, want 404", resp.StatusCode)
	}
	if carrier.transfers.Load() != 0 {
		t.Fatal("a non-attachment path reached the carrier")
	}

	resp = do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/backend/mini/attachments/thread-1/att-2?ticket=abc"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attachment path answered %d, want the carry", resp.StatusCode)
	}
	if got, _ := carrier.lastPath.Load().(string); got != "/backend/mini/attachments/thread-1/att-2" {
		t.Errorf("carrier saw path %q, want the request's own", got)
	}
}

// TestAttachedUpgradeReachesTheCarrier pins that an admitted upgrade is
// handed over rather than answered here.
func TestAttachedUpgradeReachesTheCarrier(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/ws/backend/mini"))
	_ = resp.Body.Close()
	if carrier.upgrades.Load() != 1 {
		t.Fatalf("carrier saw %d upgrades, want 1", carrier.upgrades.Load())
	}
}

// TestBootstrapNamesTheAttachedBackends — the page never composes these
// URLs, so the manifest has to publish them, and reachability must not be
// among them: probing every attached machine to answer one page load
// would make a boot as slow as the slowest sleeping laptop.
func TestBootstrapNamesTheAttachedBackends(t *testing.T) {
	f, _ := newAttachedFixture(t)
	resp := getBootstrap(t, f.srv.Addr())
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	var got Bootstrap
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if len(got.Backends) != 1 {
		t.Fatalf("backends = %+v, want one row", got.Backends)
	}
	row := got.Backends[0]
	if row.ID != "mini" || row.BackendID != "store-uuid-mini" || row.Name != "The Mini" || row.Nickname != "The Mini" {
		t.Errorf("row = %+v, want the attached profile", row)
	}
	if row.WSURL != "ws://"+f.srv.Addr()+"/ws/backend/mini" {
		t.Errorf("wsUrl = %q, want this listener's own", row.WSURL)
	}
	if row.BootstrapURL != "/bootstrap/mini.json" {
		t.Errorf("bootstrapUrl = %q, want a same-origin path", row.BootstrapURL)
	}
	if strings.Contains(string(body), "reachab") {
		t.Error("the manifest published a reachability claim; nothing probes to answer a page load")
	}
}

// TestBootstrapOmitsTheAttachedBackendsWithoutAny — absence is the shape
// every client had before attaching existed, and it must stay the shape a
// backend with no pairings answers with.
func TestBootstrapOmitsTheAttachedBackendsWithoutAny(t *testing.T) {
	f := newServerFixture(t)
	resp := getBootstrap(t, f.srv.Addr())
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if strings.Contains(string(body), "backends") {
		t.Errorf("bootstrap carried a backends key with none attached: %s", body)
	}
}
