package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
)

const probeTestToken = "test-token"

// probeClock is a fake clock the probe sleeps on, so a boot of minutes
// runs in milliseconds against a real HTTP backend that reads the same
// clock.
type probeClock struct{ ns atomic.Int64 }

var probeEpoch = time.Unix(1_700_000_000, 0)

func newProbeClock() *probeClock {
	c := &probeClock{}
	c.ns.Store(probeEpoch.UnixNano())
	return c
}

func (c *probeClock) now() time.Time { return time.Unix(0, c.ns.Load()) }

func (c *probeClock) elapsed() time.Duration { return c.now().Sub(probeEpoch) }

func (c *probeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.ns.Add(int64(d))
	return nil
}

func (c *probeClock) config() ProbeConfig {
	return ProbeConfig{AttemptTimeout: time.Second, now: c.now, sleep: c.sleep}
}

// boundedContext caps a fake-clock probe in real time, so a probe that
// never gives up fails the test instead of hanging it.
func boundedContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// probeBackend serves /bootstrap.json from answer, which sees the time
// since the probe started. It refuses a request without the launch token
// in the Authorization header.
func probeBackend(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (port int, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/bootstrap.json" || r.Header.Get("Authorization") != "Bearer "+probeTestToken || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return serverPort(t, server), requests
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return port
}

func writeManifest(w http.ResponseWriter, r *http.Request) {
	_, port, _ := net.SplitHostPort(r.Host)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"wsUrl":"ws://127.0.0.1:%s/ws"}`, port)
}

// migratingReport is a backend in its migrations: one step every 10 s,
// its database growing (updatedAt) and its heartbeat (aliveAt) every
// second.
func migratingReport(elapsed time.Duration) startupprogress.Progress {
	step := int(elapsed/(10*time.Second)) + 1
	return startupprogress.Progress{
		Phase:     "store.migrate",
		Detail:    fmt.Sprintf("Applying migration %d of 12 v%d", step, 100+step),
		Step:      step,
		Steps:     12,
		StartedAt: probeEpoch.UnixMilli(),
		UpdatedAt: atSecond(elapsed),
		AliveAt:   atSecond(elapsed),
	}
}

// atSecond is the backend clock's millis at elapsed, on a whole second.
func atSecond(elapsed time.Duration) int64 {
	return probeEpoch.Add(elapsed.Truncate(time.Second)).UnixMilli()
}

// reportAt is p with its last progress at progressed and its last
// heartbeat at alive.
func reportAt(p startupprogress.Progress, progressed, alive time.Duration) startupprogress.Progress {
	p.UpdatedAt, p.AliveAt = atSecond(progressed), atSecond(alive)
	return p
}

func TestProbeBootstrapKeepsWaitingWhileProgressAdvances(t *testing.T) {
	clock := newProbeClock()
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if clock.elapsed() >= 90*time.Second {
			writeManifest(w, r)
			return
		}
		startupprogress.Write(w, migratingReport(clock.elapsed()))
	})
	var reports []startupprogress.Progress
	cfg := clock.config()
	cfg.OnProgress = func(p startupprogress.Progress) { reports = append(reports, p) }

	if err := ProbeBootstrap(boundedContext(t), port, probeTestToken, cfg); err != nil {
		t.Fatalf("a boot reporting progress for 90 s failed at %s: %v", clock.elapsed(), err)
	}
	if got := clock.elapsed(); got < 90*time.Second {
		t.Fatalf("probe returned at %s, before the backend was ready", got)
	}
	if len(reports) == 0 {
		t.Fatal("OnProgress saw no reports")
	}
	if first, last := reports[0].Step, reports[len(reports)-1].Step; first != 1 || last != 9 {
		t.Fatalf("OnProgress saw steps %d to %d, want 1 to 9", first, last)
	}
}

// TestProbeBootstrapKeepsWaitingWhileOnlyTheDatabaseGrows: one long step
// whose statement keeps writing (the backend advances updatedAt when its
// database or WAL changes size) is progress without a new step or detail.
func TestProbeBootstrapKeepsWaitingWhileOnlyTheDatabaseGrows(t *testing.T) {
	clock := newProbeClock()
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if clock.elapsed() >= 90*time.Second {
			writeManifest(w, r)
			return
		}
		startupprogress.Write(w, reportAt(migratingReport(0), clock.elapsed(), clock.elapsed()))
	})
	if err := ProbeBootstrap(boundedContext(t), port, probeTestToken, clock.config()); err != nil {
		t.Fatalf("a step whose database kept growing for 90 s failed at %s: %v", clock.elapsed(), err)
	}
}

// TestProbeBootstrapFailsWhenOnlyTheHeartbeatAdvances: a backend that keeps
// answering and heartbeating but writes nothing and advances nothing is
// stalled. It fails 30 s after its last progress and names the phase.
func TestProbeBootstrapFailsWhenOnlyTheHeartbeatAdvances(t *testing.T) {
	clock := newProbeClock()
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		// Progresses for 10 s, then only heartbeats.
		startupprogress.Write(w, reportAt(migratingReport(min(clock.elapsed(), 10*time.Second)), min(clock.elapsed(), 10*time.Second), clock.elapsed()))
	})

	err := ProbeBootstrap(boundedContext(t), port, probeTestToken, clock.config())
	var stalled *BackendStalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("error = %v, want BackendStalledError", err)
	}
	if !errors.Is(err, ErrBackendNotReady) || errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("error %v must read as not ready, never unreachable", err)
	}
	if stalled.Unresponsive || stalled.Progress.Phase != "store.migrate" || stalled.Progress.Step != 2 || stalled.Quiet < bootstrapProbeDeadline {
		t.Fatalf("stall = %+v, want no progress in the last report (store.migrate step 2) after the deadline, while responding", stalled)
	}
	if msg := err.Error(); !strings.Contains(msg, "no progress") || !strings.Contains(msg, "store.migrate") {
		t.Fatalf("error %q does not say no progress in the phase", msg)
	}
	// The last progress was at 10 s; the stall fails 30 s later, within one
	// poll interval.
	if got, want := clock.elapsed(), 40*time.Second; got < want || got > want+bootstrapProbePollInterval {
		t.Fatalf("stalled probe failed at %s, want %s", got, want)
	}
}

// TestProbeBootstrapFailsWhenTheHeartbeatStops: a backend that keeps
// answering the same report, heartbeat included, stopped responding. It is
// the same failure at the same deadline, with its own message.
func TestProbeBootstrapFailsWhenTheHeartbeatStops(t *testing.T) {
	clock := newProbeClock()
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		startupprogress.Write(w, migratingReport(min(clock.elapsed(), 10*time.Second)))
	})

	err := ProbeBootstrap(boundedContext(t), port, probeTestToken, clock.config())
	var stalled *BackendStalledError
	if !errors.As(err, &stalled) || !errors.Is(err, ErrBackendNotReady) {
		t.Fatalf("error = %v, want BackendStalledError", err)
	}
	if !stalled.Unresponsive || stalled.Progress.Phase != "store.migrate" {
		t.Fatalf("stall = %+v, want an unresponsive backend in store.migrate", stalled)
	}
	if msg := err.Error(); !strings.Contains(msg, "backend stopped responding") || !strings.Contains(msg, "store.migrate") {
		t.Fatalf("error %q does not say the backend stopped responding in the phase", msg)
	}
	if got, want := clock.elapsed(), 40*time.Second; got < want || got > want+bootstrapProbePollInterval {
		t.Fatalf("unresponsive probe failed at %s, want %s", got, want)
	}
}

// TestProbeBootstrapBareServiceUnavailableKeepsItsDeadline: a backend that
// answers the bare 503 (no progress reports) fails 30 s after the probe
// began, as before progress existed.
func TestProbeBootstrapBareServiceUnavailableKeepsItsDeadline(t *testing.T) {
	clock := newProbeClock()
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend not ready", http.StatusServiceUnavailable)
	})

	err := ProbeBootstrap(boundedContext(t), port, probeTestToken, clock.config())
	var stalled *BackendStalledError
	if !errors.Is(err, ErrBackendNotReady) || errors.As(err, &stalled) || errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("error = %v, want the plain not-ready failure", err)
	}
	if got := clock.elapsed(); got < bootstrapProbeDeadline || got > bootstrapProbeDeadline+bootstrapProbePollInterval {
		t.Fatalf("bare 503 failed at %s, want %s", got, bootstrapProbeDeadline)
	}
}

// TestProbeBootstrapBackendGoneAfterReportingIsStalled: a backend that
// reported progress and then stopped answering was reachable, so the
// failure is that it stopped responding in its last phase, not an
// unreachable port that a fresh port could fix.
func TestProbeBootstrapBackendGoneAfterReportingIsStalled(t *testing.T) {
	clock := newProbeClock()
	var server *httptest.Server
	var once sync.Once
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startupprogress.Write(w, migratingReport(clock.elapsed()))
		if clock.elapsed() >= 5*time.Second {
			once.Do(func() { go server.Close() })
		}
	}))
	t.Cleanup(server.Close)

	err := ProbeBootstrap(boundedContext(t), serverPort(t, server), probeTestToken, clock.config())
	var stalled *BackendStalledError
	if !errors.As(err, &stalled) || errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("error = %v, want a stall", err)
	}
	if !stalled.Unresponsive || stalled.Progress.Phase != "store.migrate" || stalled.Last == nil {
		t.Fatalf("stall = %+v, want an unresponsive backend, its last phase and the transport error", stalled)
	}
}

func TestProbeBootstrapStopsWhenCanceled(t *testing.T) {
	port, requests := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		startupprogress.Write(w, migratingReport(0))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ProbeConfig{PollInterval: time.Millisecond, InitialPollInterval: time.Millisecond}
	cfg.OnProgress = func(startupprogress.Progress) {
		if requests.Load() >= 3 {
			cancel()
		}
	}
	done := make(chan error, 1)
	go func() { done <- ProbeBootstrap(ctx, port, probeTestToken, cfg) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe did not stop after cancellation")
	}
}

// A cancel that lands while the probe waits between polls ends the wait,
// not the next poll.
func TestProbeBootstrapStopsWhenCanceledDuringAWait(t *testing.T) {
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		startupprogress.Write(w, migratingReport(0))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ProbeConfig{PollInterval: time.Hour, InitialPollInterval: time.Hour}
	cfg.OnProgress = func(startupprogress.Progress) { cancel() }
	done := make(chan error, 1)
	go func() { done <- ProbeBootstrap(ctx, port, probeTestToken, cfg) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe kept waiting after cancellation")
	}
}

func TestProbeBootstrapRetriesServiceUnavailable(t *testing.T) {
	var attempts atomic.Int32
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "backend not ready", http.StatusServiceUnavailable)
			return
		}
		writeManifest(w, r)
	})
	err := ProbeBootstrap(context.Background(), port, probeTestToken, ProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ProbeBootstrap: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// TestProbeBootstrapAnsweredFailuresAreTerminal: once the backend answered
// anything but 200 or 503 the port is reachable, the answer is final, and
// it is never classified as unreachable.
func TestProbeBootstrapAnsweredFailuresAreTerminal(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			port, requests := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "no", status)
			})
			err := ProbeBootstrap(context.Background(), port, probeTestToken, ProbeConfig{Deadline: time.Second, PollInterval: time.Millisecond})
			var httpErr BootstrapHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status || errors.Is(err, ErrBackendUnreachable) {
				t.Fatalf("error = %v, want BootstrapHTTPError %d", err, status)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

func TestProbeBootstrapRejectsInvalidSuccessBody(t *testing.T) {
	port, requests := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		// wsUrl names a port this responder does not listen on, so the
		// manifest cannot be the backend this launcher booted.
		_, _ = w.Write([]byte(`{"wsUrl":"ws://127.0.0.1:1/ws"}`))
	})
	err := ProbeBootstrap(context.Background(), port, probeTestToken, ProbeConfig{Deadline: time.Second, PollInterval: time.Millisecond})
	if !errors.Is(err, ErrInvalidBootstrap) {
		t.Fatalf("error = %v, want invalid bootstrap failure", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("attempts = %d, want invalid 200 to be terminal", got)
	}
}

// TestProbeBootstrapUnreachable: nothing listens on the probed port, which
// is what a Hyper-V excluded port range looks like from Windows while the
// backend serves inside the distro.
func TestProbeBootstrapUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	clock := newProbeClock()
	err = ProbeBootstrap(boundedContext(t), port, probeTestToken, clock.config())
	if !errors.Is(err, ErrBackendUnreachable) || errors.Is(err, ErrBackendNotReady) {
		t.Fatalf("error = %v, want ErrBackendUnreachable", err)
	}
	if got := clock.elapsed(); got < bootstrapProbeDeadline || got > bootstrapProbeDeadline+bootstrapProbePollInterval {
		t.Fatalf("unreachable probe failed at %s, want %s", got, bootstrapProbeDeadline)
	}
}

// The first retries are cheap (an instant 503 while the backend finishes
// starting), so the gap starts short and grows toward the cap.
func TestProbeBootstrapBacksOffFromTheInitialInterval(t *testing.T) {
	var attempts atomic.Int32
	port, _ := probeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeManifest(w, r)
	})
	clock := newProbeClock()
	var gaps []time.Duration
	cfg := clock.config()
	cfg.sleep = func(ctx context.Context, d time.Duration) error {
		gaps = append(gaps, d)
		return clock.sleep(ctx, d)
	}
	if err := ProbeBootstrap(boundedContext(t), port, probeTestToken, cfg); err != nil {
		t.Fatalf("ProbeBootstrap: %v", err)
	}
	if len(gaps) != 2 || gaps[0] != bootstrapProbeInitialPollInterval || gaps[1] != 2*bootstrapProbeInitialPollInterval {
		t.Fatalf("retry gaps = %v, want %s then %s", gaps, bootstrapProbeInitialPollInterval, 2*bootstrapProbeInitialPollInterval)
	}
}
