package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/wsllauncher"
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
		Step: 3, Steps: 7, StartedAt: 1000, UpdatedAt: 4000, AliveAt: 5000, UpdatingTo: "1.2.3",
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
		"step": 3.0, "steps": 7.0, "startedAt": 1000.0, "updatedAt": 4000.0, "aliveAt": 5000.0, "updatingTo": "1.2.3",
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

// TestStartupReporter_PhasesNestAndHeartbeat: reports advance UpdatedAt and
// AliveAt; an open phase heartbeats AliveAt alone; ending an inner phase
// restores its parent's report; ending the outermost stops the heartbeat,
// so a backend wedged between phases stops advancing both.
func TestStartupReporter_PhasesNestAndHeartbeat(t *testing.T) {
	srv := &Server{}
	var clock atomic.Int64
	clock.Store(1_000)
	now := func() time.Time { return time.UnixMilli(clock.Add(1)) }
	r := newStartupReporter(srv, "2.0.0", 2*time.Millisecond, now)
	// A process that does no work: this test's own CPU would clear a
	// 2 ms tick's threshold.
	r.sampleWork = func() (processWork, error) { return processWork{}, nil }

	initial := progressOf(srv)
	if initial.Phase != "starting" || initial.UpdatingTo != "2.0.0" || initial.StartedAt == 0 || initial.AliveAt != initial.UpdatedAt {
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
	if got.UpdatedAt <= initial.UpdatedAt || got.AliveAt < got.UpdatedAt {
		t.Fatalf("a report did not advance UpdatedAt and AliveAt: %+v after %+v", got, initial)
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

	before := progressOf(srv)
	if !waitFor(func() bool { return progressOf(srv).AliveAt > before.AliveAt+2 }, 5*time.Second) {
		t.Fatal("an open phase did not heartbeat")
	}
	if got := progressOf(srv); got.UpdatedAt != before.UpdatedAt {
		t.Fatalf("the heartbeat advanced UpdatedAt from %d to %d with nothing progressing", before.UpdatedAt, got.UpdatedAt)
	}

	endOuter()
	stopped := progressOf(srv)
	time.Sleep(20 * time.Millisecond)
	if after := progressOf(srv); after.AliveAt != stopped.AliveAt || after.UpdatedAt != stopped.UpdatedAt {
		t.Fatalf("the report advanced from %+v to %+v with no phase open", stopped, after)
	}
	endOuter()

	// A later phase starts a fresh heartbeat.
	end := r.BeginBootPhase("app.start", "Starting services")
	before = progressOf(srv)
	if !waitFor(func() bool { return progressOf(srv).AliveAt > before.AliveAt+2 }, 5*time.Second) {
		t.Fatal("a later phase did not heartbeat")
	}
	end()

	var nilReporter *StartupReporter
	nilReporter.BeginBootPhase("x", "y")()
	nilReporter.BootPhaseDetail("z", 1, 1)
	nilReporter.WatchBootFiles("w")
}

// TestStartupReporter_WatchedFileSizeChangesAreProgress: a heartbeat that
// finds a watched file created, grown, shrunk or removed advances
// UpdatedAt; one that finds every file unchanged advances only AliveAt. A
// file that cannot be read is no evidence either way.
func TestStartupReporter_WatchedFileSizeChangesAreProgress(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	wal := db + "-wal"
	unreadable := filepath.Join(dir, "guarded.db-wal")
	if err := os.WriteFile(db, []byte("header"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	var clock atomic.Int64
	clock.Store(1_000)
	now := func() time.Time { return time.UnixMilli(clock.Add(1)) }
	// The ticker never fires; the test drives each heartbeat.
	r := newStartupReporter(srv, "", time.Hour, now)
	// While denied, stat of the guarded file fails with a permission
	// error, which is neither a size nor absence.
	var denied atomic.Bool
	denied.Store(true)
	r.stat = func(path string) (fs.FileInfo, error) {
		if path == unreadable && denied.Load() {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		}
		return os.Stat(path)
	}
	r.WatchBootFiles(db, wal, unreadable)
	end := r.BeginBootPhase("store.migrate", "Applying migration 1 of 1 rebuild")
	defer end()

	beat := func(what string, progressed bool) {
		t.Helper()
		before := progressOf(srv)
		r.beat()
		after := progressOf(srv)
		if after.AliveAt <= before.AliveAt {
			t.Fatalf("%s: AliveAt did not advance: %+v", what, after)
		}
		if moved := after.UpdatedAt != before.UpdatedAt; moved != progressed {
			t.Fatalf("%s: UpdatedAt moved = %v, want %v (%d -> %d)", what, moved, progressed, before.UpdatedAt, after.UpdatedAt)
		}
	}
	appendTo := func(path, data string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(data); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	beat("nothing written", false)
	beat("still nothing written", false)
	appendTo(wal, "frame")
	beat("the WAL appeared", true)
	beat("the WAL is unchanged", false)
	appendTo(wal, "another frame")
	beat("the WAL grew", true)
	if err := os.Truncate(wal, 0); err != nil {
		t.Fatal(err)
	}
	beat("the WAL was checkpointed to empty", true)
	appendTo(db, "page")
	beat("the database grew", true)
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	beat("the WAL was removed", true)

	// The unreadable file becomes readable. Its first readable size is
	// the baseline, not progress; a later change is.
	appendTo(unreadable, "frame")
	beat("a file written while unreadable", false)
	denied.Store(false)
	beat("the unreadable file became readable", false)
	appendTo(unreadable, "frame")
	beat("the once-unreadable file grew", true)

	// A readable file that becomes unreadable keeps its last known size:
	// losing sight of it is not progress, and neither is finding it again
	// at that size.
	denied.Store(true)
	beat("a readable file became unreadable", false)
	denied.Store(false)
	beat("it became readable at its last known size", false)
}

// TestStartupReporter_LauncherProbeJudgesProgressNotHeartbeat drives the
// Windows launcher's probe against this reporter, with its real process
// sampler, through a real server. A step blocked on a channel heartbeats
// but does no work, and fails at the deadline naming the phase. A step
// that only grows the WAL, or only spins the CPU, never fails and
// connects once the boot finishes.
func TestStartupReporter_LauncherProbeJudgesProgressNotHeartbeat(t *testing.T) {
	const (
		// Long enough that the test process's own idle CPU (the probe and
		// the server answering it) stays far under a twentieth of a tick.
		heartbeat = 100 * time.Millisecond
		deadline  = 800 * time.Millisecond
	)
	probe := func(srv *Server) (time.Duration, error) {
		_, portText, err := net.SplitHostPort(srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		started := time.Now()
		// 50 ms polls stay inside the bootstrap route's per-peer burst for
		// the longest case.
		err = wsllauncher.ProbeBootstrap(ctx, port, "test-token", wsllauncher.ProbeConfig{
			Deadline: deadline, PollInterval: 50 * time.Millisecond, AttemptTimeout: time.Second,
		})
		return time.Since(started), err
	}

	t.Run("blocked on a channel", func(t *testing.T) {
		srv := newGatedServer(t)
		dir := t.TempDir()
		db := filepath.Join(dir, "app.db")
		r := newStartupReporter(srv, "", heartbeat, time.Now)
		r.WatchBootFiles(db, db+"-wal")
		end := r.BeginBootPhase("store.migrate", "Applying migration 1 of 1 rebuild")
		defer end()
		// The step waits on a lock that is never released.
		release := make(chan struct{})
		defer close(release)
		blocked := make(chan struct{})
		go func() {
			close(blocked)
			<-release
		}()
		<-blocked

		took, err := probe(srv)
		var stalled *wsllauncher.BackendStalledError
		if !errors.As(err, &stalled) || stalled.Unresponsive || stalled.Progress.Phase != "store.migrate" {
			t.Fatalf("error = %v (%+v), want no progress in store.migrate while responding", err, stalled)
		}
		if !strings.Contains(err.Error(), "store.migrate") {
			t.Fatalf("error %q does not name the phase", err)
		}
		if took < deadline {
			t.Fatalf("failed after %s, before the %s deadline", took, deadline)
		}
		if stalled.Progress.AliveAt <= stalled.Progress.UpdatedAt {
			t.Fatalf("the last report %+v shows no heartbeat past its progress", stalled.Progress)
		}
	})

	t.Run("WAL growth only", func(t *testing.T) {
		srv := newGatedServer(t)
		dir := t.TempDir()
		db := filepath.Join(dir, "app.db")
		wal := db + "-wal"
		r := newStartupReporter(srv, "", heartbeat, time.Now)
		r.WatchBootFiles(db, wal)
		end := r.BeginBootPhase("store.migrate", "Applying migration 1 of 1 rebuild")

		// One statement writing for four deadlines, then the boot ends.
		writing := 4 * deadline
		done := make(chan error, 1)
		go func() {
			f, err := os.OpenFile(wal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				done <- err
				return
			}
			stop := time.Now().Add(writing)
			for time.Now().Before(stop) {
				if _, err := f.WriteString("frame"); err != nil {
					_ = f.Close()
					done <- err
					return
				}
				time.Sleep(heartbeat / 2)
			}
			if err := f.Close(); err != nil {
				done <- err
				return
			}
			end()
			srv.MarkReady()
			done <- nil
		}()

		took, err := probe(srv)
		if werr := <-done; werr != nil {
			t.Fatalf("write the WAL: %v", werr)
		}
		if err != nil {
			t.Fatalf("a boot whose WAL kept growing failed after %s: %v", took, err)
		}
		if took < writing {
			t.Fatalf("probe returned after %s, before the boot finished at %s", took, writing)
		}
	})

	t.Run("CPU only", func(t *testing.T) {
		srv := newGatedServer(t)
		dir := t.TempDir()
		db := filepath.Join(dir, "app.db")
		r := newStartupReporter(srv, "", heartbeat, time.Now)
		r.WatchBootFiles(db, db+"-wal")
		end := r.BeginBootPhase("store.migrate", "Applying migration 1 of 1 rebuild")

		// One statement sorting in memory for four deadlines, writing
		// nothing, then the boot ends.
		working := 4 * deadline
		go func() {
			spinCPU(working)
			end()
			srv.MarkReady()
		}()

		took, err := probe(srv)
		if err != nil {
			t.Fatalf("a boot whose step kept a CPU busy failed after %s: %v", took, err)
		}
		if took < working {
			t.Fatalf("probe returned after %s, before the boot finished at %s", took, working)
		}
	})
}

// spinCPU keeps one goroutine computing for d without touching storage.
func spinCPU(d time.Duration) {
	x := uint64(1)
	for stop := time.Now().Add(d); time.Now().Before(stop); {
		for range 10_000 {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	spinSink.Store(x)
}

var spinSink atomic.Uint64

// beatOnce drives one heartbeat and checks that it advanced AliveAt, and
// UpdatedAt exactly when progressed.
func beatOnce(t *testing.T, r *StartupReporter, srv *Server, what string, progressed bool) {
	t.Helper()
	before := progressOf(srv)
	r.beat()
	after := progressOf(srv)
	if after.AliveAt <= before.AliveAt {
		t.Fatalf("%s: AliveAt did not advance: %+v", what, after)
	}
	if moved := after.UpdatedAt != before.UpdatedAt; moved != progressed {
		t.Fatalf("%s: UpdatedAt moved = %v, want %v (%d -> %d)", what, moved, progressed, before.UpdatedAt, after.UpdatedAt)
	}
}

// TestStartupReporter_ProcessWorkIsProgress: a heartbeat that finds the
// process used a twentieth of a CPU, or moved 64 KiB/s to or from
// storage, since the last one advances UpdatedAt; less advances only
// AliveAt. The first sample, a sample from a different I/O counter and
// the sample after a failed one are baselines, not progress.
func TestStartupReporter_ProcessWorkIsProgress(t *testing.T) {
	srv := &Server{}
	var clock atomic.Int64
	clock.Store(1_000)
	now := func() time.Time { return time.UnixMilli(clock.Add(1)) }
	// The ticker never fires; the test drives each heartbeat.
	r := newStartupReporter(srv, "", time.Hour, now)
	sample := processWork{cpu: time.Second, io: 4096, ioSource: "first"}
	var sampleErr error
	r.sampleWork = func() (processWork, error) { return sample, sampleErr }
	end := r.BeginBootPhase("store.migrate", "Applying migration 1 of 1 rebuild")
	defer end()
	minCPU := time.Hour / startupCPUShare
	minIO := int64(startupIOPerSecond * time.Hour.Seconds())

	beatOnce(t, r, srv, "the first sample", false)
	sample.cpu += minCPU - 1
	beatOnce(t, r, srv, "CPU under the threshold", false)
	sample.cpu += minCPU
	beatOnce(t, r, srv, "CPU at the threshold", true)
	sample.io += minIO - 1
	beatOnce(t, r, srv, "I/O under the threshold", false)
	sample.io += minIO
	beatOnce(t, r, srv, "I/O at the threshold", true)
	sample.io, sample.ioSource = sample.io+10*minIO, "second"
	beatOnce(t, r, srv, "a count from another I/O counter", false)
	sample.io += minIO
	beatOnce(t, r, srv, "I/O on the new counter", true)
	sampleErr = errors.New("unreadable")
	sample.cpu += 10 * minCPU
	beatOnce(t, r, srv, "a failed sample", false)
	sampleErr = nil
	beatOnce(t, r, srv, "the sample after a failure", false)
	sample.cpu += minCPU
	beatOnce(t, r, srv, "CPU after the failure", true)
}

// TestReadProcessWorkCountsCPUAndStorage: the platform sampler sees this
// process's CPU time grow while it computes and its storage I/O grow by
// what it writes to a file. On Linux the I/O comes from /proc/self/io.
func TestReadProcessWorkCountsCPUAndStorage(t *testing.T) {
	before, err := readProcessWork()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	// 100 ms of CPU within 1 s of computing needs a tenth of a core; a
	// sampler that counted only kernel or system time would take seconds.
	var mid processWork
	for giveUp := time.Now().Add(time.Second); ; {
		spinCPU(20 * time.Millisecond)
		if mid, err = readProcessWork(); err != nil {
			t.Fatalf("sample: %v", err)
		}
		if mid.cpu-before.cpu >= 100*time.Millisecond {
			break
		}
		if time.Now().After(giveUp) {
			t.Fatalf("CPU time grew %s in 1 s of computing, want at least 100ms", mid.cpu-before.cpu)
		}
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := readProcessWork()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if runtime.GOOS == "linux" && after.ioSource != "/proc/self/io" {
		t.Fatalf("I/O came from %q on Linux, want /proc/self/io", after.ioSource)
	}
	if after.ioSource != mid.ioSource || after.io-mid.io < 1<<20 {
		t.Fatalf("I/O went from %d (%s) to %d (%s) across a 1 MiB write", mid.io, mid.ioSource, after.io, after.ioSource)
	}
}

// TestAttachedBootstrapPassesOnAStartingReport: a carried backend that is
// starting shows its own progress through this listener, not an outage.
func TestAttachedBootstrapPassesOnAStartingReport(t *testing.T) {
	f, carrier := newAttachedFixture(t)
	carrier.manifestErr = fmt.Errorf("wrapped: %w", &BackendStartingError{Progress: startupprogress.Progress{
		Phase: "store.migrate", Detail: "Applying migration 1 of 4 x", Step: 1, Steps: 4, UpdatedAt: 42, AliveAt: 43,
	}})
	resp := do(t, attachedRequest(t, http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap/mini.json"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := readStartupBody(t, resp)
	if body["reason"] != "starting" || body["phase"] != "store.migrate" || body["steps"] != 4.0 || body["updatedAt"] != 42.0 || body["aliveAt"] != 43.0 {
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
