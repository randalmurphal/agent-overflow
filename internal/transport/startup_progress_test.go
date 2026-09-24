package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/startupprogress"
)

func newGatedServer(t *testing.T) *Server {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&fakeApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv, err := New(Config{
		Dispatcher:               d,
		EventBus:                 NewEventBus(20),
		Token:                    "test-token",
		RequireReadyForBootstrap: true,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return srv
}

func readStartupBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode starting body: %v", err)
	}
	return body
}

// TestServer_BootstrapReportsStartupProgress pins the readiness contract:
// a bare 503 until the boot reports anything, the starting body with
// no-store and Retry-After once it does, the credential check still in
// front of it, and the manifest after MarkReady.
func TestServer_BootstrapReportsStartupProgress(t *testing.T) {
	srv := newGatedServer(t)

	resp := getBootstrap(t, srv.Addr())
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("before any progress: %d %q %q, want the bare text 503", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	}

	srv.SetStartupProgress(startupprogress.Progress{
		Phase: "store.migrate", Detail: "Applying migration 3 of 7 add_index",
		Step: 3, Steps: 7, StartedAt: 1000, UpdatedAt: 4000, UpdatingTo: "1.2.3",
	})
	resp = getBootstrap(t, srv.Addr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status with progress = %d, want 503", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":  "application/json",
		"Cache-Control": "no-store, max-age=0",
		"Retry-After":   "1",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	body := readStartupBody(t, resp)
	want := map[string]any{
		"reason": "starting", "phase": "store.migrate", "detail": "Applying migration 3 of 7 add_index",
		"step": 3.0, "steps": 7.0, "startedAt": 1000.0, "updatedAt": 4000.0, "updatingTo": "1.2.3",
	}
	if fmt.Sprint(body) != fmt.Sprint(want) {
		t.Fatalf("body = %v, want %v", body, want)
	}

	refused, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=never-minted", srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("refused credential with progress set = %d, want 404 (progress is not disclosed before the credential check)", refused.StatusCode)
	}

	srv.MarkReady()
	resp = getBootstrap(t, srv.Addr())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status after MarkReady = %d, want 200", resp.StatusCode)
	}
	var manifest Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil || manifest.WSURL == "" {
		t.Fatalf("manifest after MarkReady = %+v (%v), want a wsUrl", manifest, err)
	}
}

// TestServer_ReadinessGuardHoldsAppRoutesUntilReady: a native client
// holding the launch token reaches /ws without the bootstrap, so the
// bootstrap gate alone does not keep App methods from running before
// App.Start finished. Until MarkReady, the App-facing HTTP routes close
// without an answer, and a loopback /ws serves only StartupMethods,
// answering every other RPC with the retryable temporarily_unavailable.
// The page, its assets and /healthz stay reachable.
func TestServer_ReadinessGuardHoldsAppRoutesUntilReady(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.RequireReadyForBootstrap = true
		cfg.StartupMethods = []string{"Add"}
	})
	srv := f.srv
	srv.SetStartupProgress(startupprogress.Progress{Phase: "starting"})

	appRoutes := []struct{ method, path string }{
		{http.MethodGet, "/bundle/manifest.json"},
		{http.MethodPut, "/attachments/upload"},
	}
	askApp := func(method, path string) (*http.Response, error) {
		req, err := http.NewRequest(method, "http://"+srv.Addr()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		return http.DefaultClient.Do(req)
	}
	for _, route := range appRoutes {
		if resp, err := askApp(route.method, route.path); err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s %s answered %d before MarkReady, want the connection closed", route.method, route.path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/healthz", "/", "/index.html"} {
		resp, err := http.Get("http://" + srv.Addr() + path)
		if err != nil {
			t.Fatalf("%s before MarkReady: %v, want an answer", path, err)
		}
		_ = resp.Body.Close()
	}

	conn := f.dial(t)
	resp := f.rpc(t, conn, 0, "Greet", "early")
	if resp.Error == nil || resp.Error.Code != ErrCodeTemporarilyUnavailable {
		t.Fatalf("Greet before MarkReady = %+v / %s, want temporarily_unavailable", resp.Error, resp.Result)
	}
	f.app.mu.Lock()
	calls := append([]string(nil), f.app.calls...)
	f.app.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("App methods ran before MarkReady: %v", calls)
	}
	resp = f.rpc(t, conn, 0, "Add", 2, 3)
	if resp.Error != nil || string(resp.Result) != "5" {
		t.Fatalf("startup method before MarkReady = %+v / %s, want served", resp.Error, resp.Result)
	}

	srv.MarkReady()
	for _, route := range appRoutes {
		resp, err := askApp(route.method, route.path)
		if err != nil {
			t.Fatalf("%s %s after MarkReady: %v, want an answer", route.method, route.path, err)
		}
		_ = resp.Body.Close()
	}
	resp = f.rpc(t, conn, 0, "Greet", "late")
	if resp.Error != nil || string(resp.Result) != `"hello, late"` {
		t.Fatalf("Greet after MarkReady = %+v / %s", resp.Error, resp.Result)
	}
}

// TestServedBeforeReadyAdmitsOnlyLoopbackUpgrades: an off-host upgrade
// before MarkReady closes without an answer, the transient shape, rather
// than reaching session admission while the identity that judges it is
// still opening.
func TestServedBeforeReadyAdmitsOnlyLoopbackUpgrades(t *testing.T) {
	mux := http.NewServeMux()
	for _, pattern := range []string{"/", BootstrapPath, WSPath, HealthPath, PageURLPath, "GET /bundle/manifest.json"} {
		mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	for _, c := range []struct {
		method, path, remote string
		want                 bool
	}{
		{http.MethodGet, "/ws", "127.0.0.1:5000", true},
		{http.MethodGet, "/ws", "[::1]:5000", true},
		{http.MethodGet, "/ws", "192.0.2.10:5000", false},
		{http.MethodGet, "/bootstrap.json", "192.0.2.10:5000", true},
		{http.MethodGet, "/healthz", "192.0.2.10:5000", true},
		{http.MethodGet, "/assets/app.js", "192.0.2.10:5000", true},
		{http.MethodGet, "/bundle/manifest.json", "127.0.0.1:5000", false},
	} {
		r := httptest.NewRequest(c.method, "http://127.0.0.1"+c.path, nil)
		r.RemoteAddr = c.remote
		if got := servedBeforeReady(mux, r); got != c.want {
			t.Errorf("%s %s from %s served before ready = %v, want %v", c.method, c.path, c.remote, got, c.want)
		}
	}
}

// progressOf reads what srv currently reports.
func progressOf(srv *Server) startupprogress.Progress {
	if p := srv.startupProgress.Load(); p != nil {
		return *p
	}
	return startupprogress.Progress{}
}

// TestStartupReporter_PhasesNestAndHeartbeat: an open phase advances
// UpdatedAt without new reports; ending an inner phase restores its
// parent's report; ending the outermost stops the heartbeat, so a backend
// wedged between phases stops advancing.
func TestStartupReporter_PhasesNestAndHeartbeat(t *testing.T) {
	srv := &Server{}
	var clock atomic.Int64
	clock.Store(1_000)
	now := func() time.Time { return time.UnixMilli(clock.Add(1)) }
	r := newStartupReporter(srv, "2.0.0", 2*time.Millisecond, now)

	initial := progressOf(srv)
	if initial.Phase != "starting" || initial.UpdatingTo != "2.0.0" || initial.StartedAt == 0 {
		t.Fatalf("initial report = %+v", initial)
	}

	endOuter := r.BeginBootPhase("store.open", "Opening the database")
	r.BootPhaseDetail("Applying migration 1 of 2 first", 1, 2)
	got := progressOf(srv)
	if got.Phase != "store.open" || got.Detail != "Applying migration 1 of 2 first" || got.Step != 1 || got.Steps != 2 {
		t.Fatalf("detail report = %+v", got)
	}
	if got.StartedAt != initial.StartedAt || got.UpdatingTo != "2.0.0" {
		t.Fatalf("report lost boot identity: %+v", got)
	}

	endInner := r.BeginBootPhase("store.backfill", "Backfilling")
	if got := progressOf(srv); got.Phase != "store.backfill" || got.Step != 0 {
		t.Fatalf("inner report = %+v", got)
	}
	endInner()
	endInner()
	if got := progressOf(srv); got.Phase != "store.open" || got.Step != 1 || got.Steps != 2 {
		t.Fatalf("after the inner phase ended = %+v, want the parent's report", got)
	}

	before := progressOf(srv).UpdatedAt
	if !waitFor(func() bool { return progressOf(srv).UpdatedAt > before+2 }, 5*time.Second) {
		t.Fatal("an open phase did not heartbeat")
	}

	endOuter()
	stopped := progressOf(srv).UpdatedAt
	time.Sleep(20 * time.Millisecond)
	if after := progressOf(srv).UpdatedAt; after != stopped {
		t.Fatalf("UpdatedAt advanced from %d to %d with no phase open", stopped, after)
	}
	endOuter()

	// A later phase starts a fresh heartbeat.
	end := r.BeginBootPhase("app.start", "Starting services")
	before = progressOf(srv).UpdatedAt
	if !waitFor(func() bool { return progressOf(srv).UpdatedAt > before+2 }, 5*time.Second) {
		t.Fatal("a later phase did not heartbeat")
	}
	end()

	var nilReporter *StartupReporter
	nilReporter.BeginBootPhase("x", "y")()
	nilReporter.BootPhaseDetail("z", 1, 1)
}

// TestStartupReporter_ObserveSeparatesHeartbeatsFromSteps: a trial is
// judged only by real steps, so the observer must be able to tell a
// heartbeat from a report.
func TestStartupReporter_ObserveSeparatesHeartbeatsFromSteps(t *testing.T) {
	srv := &Server{}
	var clock atomic.Int64
	now := func() time.Time { return time.UnixMilli(clock.Add(1)) }
	r := newStartupReporter(srv, "", 2*time.Millisecond, now)

	type report struct {
		detail   string
		liveness bool
	}
	var mu sync.Mutex
	var got []report
	if r.Observe(func(p startupprogress.Progress, liveness bool) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, report{p.Detail, liveness})
	}) != r {
		t.Fatal("Observe did not return its reporter")
	}
	end := r.BeginBootPhase("store.open", "Opening the database")
	r.BootPhaseDetail("Applying migration 1 of 1", 1, 1)
	heartbeats := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, g := range got {
			if g.liveness {
				n++
			}
		}
		return n
	}
	if !waitFor(func() bool { return heartbeats() >= 2 }, 5*time.Second) {
		t.Fatal("no heartbeat reached the observer")
	}
	end()

	mu.Lock()
	defer mu.Unlock()
	var steps []string
	for _, g := range got {
		if !g.liveness {
			steps = append(steps, g.detail)
		} else if g.detail != "Opening the database" && g.detail != "Applying migration 1 of 1" {
			t.Fatalf("heartbeat carried %q, want the open step", g.detail)
		}
	}
	want := []string{"Starting", "Opening the database", "Applying migration 1 of 1"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %q, want %q", steps, want)
	}

	var nilReporter *StartupReporter
	if nilReporter.Observe(func(startupprogress.Progress, bool) { t.Fatal("a nil reporter reported") }) != nil {
		t.Fatal("a nil reporter returned a reporter")
	}
}

// TestAttachedBootstrapPassesOnAStartingReport: a carried backend that is
// starting shows its own progress through this listener, not an outage.
func TestAttachedBootstrapPassesOnAStartingReport(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	carrier.manifestErr = fmt.Errorf("wrapped: %w", &BackendStartingError{Progress: startupprogress.Progress{
		Phase: "store.migrate", Detail: "Applying migration 1 of 4 x", Step: 1, Steps: 4, UpdatedAt: 42,
	}})
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap/mini.json"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := readStartupBody(t, resp)
	if body["reason"] != "starting" || body["phase"] != "store.migrate" || body["steps"] != 4.0 || body["updatedAt"] != 42.0 {
		t.Fatalf("body = %v, want the far backend's report", body)
	}

	carrier.manifestErr = errors.New("unreachable")
	resp = do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap/mini.json"))
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(raw), "starting") {
		t.Fatalf("unreachable carrier = %d %q, want the plain 503", resp.StatusCode, raw)
	}
}

// TestHelloBeforeReadyReadsNoAppState: a loopback connection admitted
// before MarkReady gets a hello without the App-supplied fields, because
// the getters behind them read state App.Start is still writing. After
// MarkReady a new connection gets all three.
func TestHelloBeforeReadyReadsNoAppState(t *testing.T) {
	var nameCalls, browserCalls, routeCalls atomic.Int32
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.RequireReadyForBootstrap = true
		cfg.BackendIdentity = func() (string, string) { return "backend-1", "gen-1" }
		cfg.BackendNameGetter = func() string { nameCalls.Add(1); return "Workstation" }
		cfg.BrowserAvailable = func() bool { browserCalls.Add(1); return true }
		cfg.ComputerRoutes = func() []computerroute.Route {
			routeCalls.Add(1)
			return []computerroute.Route{{Endpoint: "https://gpu.test.ts.net"}}
		}
	})
	readHello := func() helloFrame {
		conn := f.dial(t)
		defer conn.CloseNow()
		var hello helloFrame
		if err := json.Unmarshal(readFirstFrame(t, conn), &hello); err != nil {
			t.Fatal(err)
		}
		return hello
	}

	early := readHello()
	if nameCalls.Load() != 0 || browserCalls.Load() != 0 || routeCalls.Load() != 0 {
		t.Fatalf("hello before MarkReady read App state: name=%d browser=%d routes=%d", nameCalls.Load(), browserCalls.Load(), routeCalls.Load())
	}
	if early.BackendName != "" || len(early.Routes) != 0 || slices.Contains(early.Capabilities, CapabilityBrowser) {
		t.Fatalf("hello before MarkReady = %+v, want no name, routes or browser capability", early)
	}

	f.srv.MarkReady()
	late := readHello()
	if late.BackendName != "Workstation" || len(late.Routes) != 1 || !slices.Contains(late.Capabilities, CapabilityBrowser) {
		t.Fatalf("hello after MarkReady = %+v, want the name, routes and browser capability", late)
	}
}
