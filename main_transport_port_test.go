package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

// These tests drive the real pin → bind → adopt cycle with real
// listeners on 127.0.0.1 and a t.TempDir settings dir. They stop short
// of bootTransport itself, which owns App construction and fatalf's on
// failure; the port decision is the whole of what's under test and it
// lives in pinTransportPort / transportPortPin.adopt.

// bootOnce runs one launch's worth of port handling — pin, bind, adopt
// — and returns the port the listener actually came up on plus the
// stop func that releases it. reset is the --reset-transport-port
// signal the Windows launcher passes on its one retry. Callers that
// boot twice must stop the first server before the second call, exactly
// as two real launches would be sequential.
func bootOnce(t *testing.T, dir string, cfg transport.Config, reset bool) (port int, stop func()) {
	t.Helper()
	cfg.Dispatcher = transport.NewDispatcher()
	cfg.EventBus = transport.NewEventBus(0)
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1"
	}

	pin := pinTransportPort(&cfg, dir, reset)

	srv, err := transport.New(cfg)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport start: %v", err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("transport shutdown: %v", err)
		}
	}
	t.Cleanup(stop)

	pin.adopt(srv.Addr())
	return portFromAddr(srv.Addr()), stop
}

// readPinnedPort returns the persisted port, or -1 when the file is
// absent (distinct from a persisted zero, which must never happen).
func readPinnedPort(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, transportPortFileName))
	if os.IsNotExist(err) {
		return -1
	}
	if err != nil {
		t.Fatalf("read pinned port: %v", err)
	}
	var state transportPortState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode pinned port %q: %v", raw, err)
	}
	return state.Port
}

func writePinnedPortRaw(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, transportPortFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write pinned port: %v", err)
	}
}

// freePort binds and immediately releases a port, yielding a number
// that is very likely still free on the next bind.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	port := portFromAddr(l.Addr().String())
	if err := l.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return port
}

func TestTransportPortPersistsAndIsReusedNextBoot(t *testing.T) {
	dir := t.TempDir()

	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("expected no pin file before first boot, got port %d", got)
	}

	first, stopFirst := bootOnce(t, dir, transport.Config{}, false)
	if first == 0 {
		t.Fatal("first boot bound no port")
	}
	if got := readPinnedPort(t, dir); got != first {
		t.Fatalf("pin file holds %d, want the bound port %d", got, first)
	}
	stopFirst()

	second, _ := bootOnce(t, dir, transport.Config{}, false)
	if second != first {
		t.Fatalf("second boot bound port %d, want the pinned %d", second, first)
	}
	if got := readPinnedPort(t, dir); got != first {
		t.Fatalf("pin file changed to %d on an uneventful re-bind, want %d", got, first)
	}
}

func TestTransportPortAdoptsNewPortWhenPinnedOneIsTaken(t *testing.T) {
	dir := t.TempDir()

	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer squatter.Close()
	taken := portFromAddr(squatter.Addr().String())
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(taken)+`}`)

	bound, _ := bootOnce(t, dir, transport.Config{}, false)
	if bound == 0 {
		t.Fatal("server did not come up")
	}
	if bound == taken {
		t.Fatalf("bound the squatted port %d", taken)
	}
	if got := readPinnedPort(t, dir); got != bound {
		t.Fatalf("pin file holds %d after fallback, want the newly bound %d", got, bound)
	}
}

func TestTransportPortExplicitListenPortBypassesTheFile(t *testing.T) {
	dir := t.TempDir()
	explicit := freePort(t)

	bound, _ := bootOnce(t, dir, transport.Config{Port: explicit}, false)
	if bound != explicit {
		t.Fatalf("bound %d, want the explicitly requested %d", bound, explicit)
	}
	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("explicit --listen port wrote a pin file (port %d); it must not touch it", got)
	}
}

func TestTransportPortExplicitListenPortIgnoresAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	explicit := freePort(t)
	pinned := explicit + 1
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(pinned)+`}`)

	bound, _ := bootOnce(t, dir, transport.Config{Port: explicit}, false)
	if bound != explicit {
		t.Fatalf("bound %d, want the explicitly requested %d", bound, explicit)
	}
	if got := readPinnedPort(t, dir); got != pinned {
		t.Fatalf("explicit --listen port rewrote the pin file to %d, want it untouched at %d", got, pinned)
	}
}

func TestTransportPortRejectsInvalidFileContents(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"garbage", "not json at all"},
		{"empty", ""},
		{"zero", `{"port":0}`},
		{"negative", `{"port":-1}`},
		{"out of range", `{"port":70000}`},
		{"wrong type", `{"port":"8080"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePinnedPortRaw(t, dir, tc.body)

			if got := loadTransportPort(filepath.Join(dir, transportPortFileName)); got != 0 {
				t.Fatalf("loadTransportPort returned %d for %q, want 0 (treated as absent)", got, tc.body)
			}

			bound, _ := bootOnce(t, dir, transport.Config{}, false)
			if bound == 0 {
				t.Fatal("server did not come up")
			}
			if got := readPinnedPort(t, dir); got != bound {
				t.Fatalf("pin file holds %d, want it replaced with the bound port %d", got, bound)
			}
		})
	}
}

func TestTransportPortNoSettingsDirStaysEphemeral(t *testing.T) {
	bound, _ := bootOnce(t, "", transport.Config{}, false)
	if bound == 0 {
		t.Fatal("server did not come up without a settings dir")
	}
}

// TestTransportPortResetDiscardsThePinAndAdoptsANewOne covers the
// launcher's escape hatch: a pin that binds perfectly inside WSL but is
// unreachable from the Windows host (a Hyper-V excluded port range,
// re-seeded on every Windows reboot) would otherwise be honoured on
// every launch, failing identically forever. --reset-transport-port
// removes it BEFORE the bind, so the boot asks for an ephemeral port
// and adopt() pins whatever it lands on.
//
// Driven through pinTransportPort/adopt rather than a real bind because
// the assertion is "the pinned port is not requested" — a real bind
// could coincidentally be handed the same number back.
func TestTransportPortResetDiscardsThePinAndAdoptsANewOne(t *testing.T) {
	dir := t.TempDir()
	pinned := freePort(t)
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(pinned)+`}`)

	cfg := transport.Config{BindAddr: "127.0.0.1"}
	pin := pinTransportPort(&cfg, dir, true)

	if cfg.Port != 0 {
		t.Fatalf("reset boot still asked for port %d, want an ephemeral bind", cfg.Port)
	}
	if cfg.EphemeralPortFallback {
		t.Error("reset boot armed the pinned-port fallback; there is no pin to fall back from")
	}
	if pin.requested != 0 {
		t.Errorf("pin.requested = %d after a reset, want 0", pin.requested)
	}
	if pin.dir != dir {
		t.Errorf("pin.dir = %q, want %q — a reset must still adopt the port it lands on", pin.dir, dir)
	}
	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("reset left a pin file behind holding %d", got)
	}

	// The new port is adopted, so exactly one launch churns the origin.
	adopted := pinned + 1
	pin.adopt("127.0.0.1:" + strconv.Itoa(adopted))
	if got := readPinnedPort(t, dir); got != adopted {
		t.Fatalf("pin file holds %d after adopt, want the newly bound %d", got, adopted)
	}
}

// A reset with nothing to reset is a normal boot: the flag is passed
// unconditionally by the launcher's retry, and a first launch (or one
// whose pin was already cleared) must not be a special case.
func TestTransportPortResetWithoutAPinBootsNormally(t *testing.T) {
	dir := t.TempDir()

	bound, _ := bootOnce(t, dir, transport.Config{}, true)
	if bound == 0 {
		t.Fatal("server did not come up")
	}
	if got := readPinnedPort(t, dir); got != bound {
		t.Fatalf("pin file holds %d, want the bound port %d", got, bound)
	}
}

// An explicit --listen port owns the bind outright, and the pin file is
// not consulted on such a boot — so a reset must not delete it either.
// Otherwise one `--listen host:port` run would silently cost the desktop
// launches their stable origin.
func TestTransportPortResetLeavesTheFileAloneForAnExplicitPort(t *testing.T) {
	dir := t.TempDir()
	explicit := freePort(t)
	pinned := explicit + 1
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(pinned)+`}`)

	bound, _ := bootOnce(t, dir, transport.Config{Port: explicit}, true)
	if bound != explicit {
		t.Fatalf("bound %d, want the explicitly requested %d", bound, explicit)
	}
	if got := readPinnedPort(t, dir); got != pinned {
		t.Fatalf("reset deleted the pin on an explicit-port boot (file now %d, want %d)", got, pinned)
	}
}

// clearTransportPort is best-effort on purpose: an unremovable file must
// not stop the boot that follows it.
func TestClearTransportPortToleratesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	clearTransportPort(dir, "test")
	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("clearTransportPort created a file holding %d", got)
	}
}

func TestTransportPortConfigRejectsOutOfRangePort(t *testing.T) {
	if _, err := transport.New(transport.Config{
		Dispatcher: transport.NewDispatcher(),
		EventBus:   transport.NewEventBus(0),
		Port:       70000,
	}); err == nil {
		t.Fatal("transport.New accepted an out-of-range port")
	}
}
