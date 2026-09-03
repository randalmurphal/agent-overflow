package main

import (
	"context"
	"encoding/json"
	"errors"
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
//
// The saved network.listenPort is zero here; bootOnceWithSettingsPort is
// the same launch with one, so the cases for the third precedence input
// read as what they add.
func bootOnce(t *testing.T, dir string, cfg transport.Config, reset bool) (port int, stop func()) {
	t.Helper()
	return bootOnceWithSettingsPort(t, dir, cfg, 0, reset)
}

func bootOnceWithSettingsPort(t *testing.T, dir string, cfg transport.Config, settingsPort int, reset bool) (port int, stop func()) {
	t.Helper()
	cfg.Dispatcher = transport.NewDispatcher()
	cfg.EventBus = transport.NewEventBus(0)
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1"
	}

	pin := pinTransportPort(&cfg, dir, settingsPort, reset)

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
	pin := pinTransportPort(&cfg, dir, 0, true)

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

// The three inputs in precedence order: --listen, then the saved
// network.listenPort, then the pin cache. Each case names the pair it
// separates, because the whole value of the middle one is that it sits
// between two behaviours it shares nothing with.

func TestTransportPortSettingsPortBeatsTheCache(t *testing.T) {
	dir := t.TempDir()
	cached := freePort(t)
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(cached)+`}`)
	chosen := freePort(t)
	if chosen == cached {
		t.Skip("the two probe ports collided; nothing to separate")
	}

	bound, _ := bootOnceWithSettingsPort(t, dir, transport.Config{}, chosen, false)
	if bound != chosen {
		t.Fatalf("bound %d, want the saved network.listenPort %d", bound, chosen)
	}
}

// The cache-coherence half: a port the setting asked for and the kernel
// gave IS the previous bind, so clearing the setting later means "stay
// here" rather than "jump back to a number from before you set this".
func TestTransportPortSettingsPortIsAdoptedIntoTheCache(t *testing.T) {
	dir := t.TempDir()
	stale := freePort(t)
	writePinnedPortRaw(t, dir, `{"port":`+strconv.Itoa(stale)+`}`)
	chosen := freePort(t)
	if chosen == stale {
		t.Skip("the two probe ports collided; nothing to separate")
	}

	bound, _ := bootOnceWithSettingsPort(t, dir, transport.Config{}, chosen, false)
	if got := readPinnedPort(t, dir); got != bound {
		t.Fatalf("the cache holds %d after binding the saved port %d; it must name the previous bind", got, bound)
	}
}

// An explicit --listen is one launch and the setting is the install, so
// the flag wins for the BIND and writes nothing: a debugging run must not
// rewrite where this install lives.
func TestTransportPortExplicitListenBeatsTheSettingsPort(t *testing.T) {
	dir := t.TempDir()
	explicit := freePort(t)
	saved := explicit + 1

	bound, _ := bootOnceWithSettingsPort(t, dir, transport.Config{Port: explicit}, saved, false)
	if bound != explicit {
		t.Fatalf("bound %d, want the explicitly requested %d", bound, explicit)
	}
	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("an explicit --listen wrote a pin file (port %d) while a settings port was saved", got)
	}
}

// The setting deliberately takes no ephemeral fallback. The whole reason
// to set it is that every share URL names the number, so a backend that
// quietly moved would be unreachable at the only address anybody has.
func TestTransportPortSettingsPortDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer squatter.Close()
	taken := portFromAddr(squatter.Addr().String())

	cfg := transport.Config{
		Dispatcher: transport.NewDispatcher(),
		EventBus:   transport.NewEventBus(0),
		BindAddr:   "127.0.0.1",
	}
	pin := pinTransportPort(&cfg, dir, taken, false)
	if cfg.Port != taken {
		t.Fatalf("cfg.Port = %d, want the saved %d", cfg.Port, taken)
	}
	if cfg.EphemeralPortFallback {
		t.Fatal("a saved network.listenPort took the cache's ephemeral fallback; it must fail loudly instead")
	}

	srv, err := transport.New(cfg)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		t.Fatal("Start succeeded on an occupied saved port; the bind must fail so the operator is told")
	}

	// And the failure says nothing about the cache, which is unrelated to
	// a port the setting named.
	pin.clearOnFailedBind(errors.New("bind failed"))
	if got := readPinnedPort(t, dir); got != -1 {
		t.Fatalf("a failed settings-port bind touched the cache (file now %d)", got)
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
