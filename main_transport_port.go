package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transport"
)

// transportPortFileName holds this installation's pinned listen port,
// next to client-id.json in the boot settings dir.
const transportPortFileName = "transport-port.json"

// transportPortState is the on-disk shape of transportPortFileName.
type transportPortState struct {
	Port int `json:"port"`
}

// transportPortPin carries the pre-bind persisted-port decision to the
// post-bind adoption. The zero value is inert — adopt() does nothing —
// which is exactly what an explicit `--listen host:port` or an
// unresolvable settings dir yields.
type transportPortPin struct {
	// dir is the settings dir to persist into. Empty means "don't".
	dir string
	// requested is the port we asked the transport for: 0 on first boot
	// (or after a rejected file), otherwise the persisted value.
	requested int
}

// pinTransportPort resolves the listen port when the config would
// otherwise take an ephemeral one, and returns the pin the caller
// adopts from once the listener is actually bound.
//
// A zero cfg.Port is the desktop default AND what `--listen 127.0.0.1:0`
// (the Windows WSL launcher) resolves to; both want a stable port,
// because the embedded webview's origin is host+port and every
// origin-scoped store the frontend owns — localStorage, the IndexedDB
// thread replica (docs/architecture/thread-replica-sync.md §6.0) — is wiped
// when it changes. An explicit non-zero port is the operator's choice
// and is left alone, file untouched.
//
// This is deliberately a preference, never a requirement: the transport
// falls back to an ephemeral port when the pinned one is unavailable,
// and adopt() then re-pins whatever it got, so a permanently squatted
// port costs one churned origin rather than one per launch.
//
// reset is the caller's "this pin is bad, forget it" signal
// (--reset-transport-port). It exists because a pin can be honoured
// perfectly and still be unusable: the WSL backend binds it inside the
// distro while the Windows host cannot reach it, which is what a
// Hyper-V/WSL2 excluded port range does to a port in the ephemeral
// range — and those ranges are re-seeded on every Windows reboot, so an
// adopted pin that worked yesterday can be dead today, identically, on
// every launch. Only the party that can observe the failure (the
// launcher, from the Windows side) can say so, so the reset is an
// explicit signal rather than anything this process could infer.
func pinTransportPort(cfg *transport.Config, dir string, reset bool) transportPortPin {
	if cfg.Port != 0 {
		if reset {
			log.Printf("transport-port: --%s has nothing to do: --listen named port %d explicitly, so no pin is consulted or written", resetTransportPortFlag, cfg.Port)
		}
		return transportPortPin{}
	}
	if dir == "" {
		log.Printf("transport-port: no settings dir resolvable; using an ephemeral port (browser-side storage resets each launch)")
		return transportPortPin{}
	}
	if reset {
		clearTransportPort(dir, fmt.Sprintf("--%s was passed", resetTransportPortFlag))
	}
	port := loadTransportPort(filepath.Join(dir, transportPortFileName))
	if port != 0 {
		cfg.Port = port
		cfg.EphemeralPortFallback = true
	}
	return transportPortPin{dir: dir, requested: port}
}

// adopt records the port the listener actually bound, so the next boot
// asks for it. Called with Server.Addr() after Start.
//
// Adopt-on-fallback is the whole point: when the requested port was
// taken we persist the NEW one rather than keeping a pin we can never
// honor. Persisting is best-effort — a failure costs a stable origin,
// never a boot.
func (p transportPortPin) adopt(addr string) {
	if p.dir == "" {
		return
	}
	bound := portFromAddr(addr)
	if bound == 0 {
		log.Printf("transport-port: bound address %q carries no port; leaving the pin alone", addr)
		return
	}
	if bound == p.requested {
		return
	}
	if p.requested != 0 {
		log.Printf("transport-port: pinned port %d unavailable; adopting %d", p.requested, bound)
	}
	storeTransportPort(p.dir, bound)
}

// clearOnFailedBind removes the pin file after a Start that failed
// while a pinned port was requested. The in-server ephemeral fallback
// absorbs ordinary taken-port failures, so reaching this means an error
// class its predicate missed — and keeping the pin would replay the
// identical failure on every launch. Ephemeral next boot beats bricked.
func (p transportPortPin) clearOnFailedBind(bindErr error) {
	if p.dir == "" || p.requested == 0 {
		return
	}
	clearTransportPort(p.dir, fmt.Sprintf("bind failed (%v)", bindErr))
}

// clearTransportPort removes the pin file and says why. Both callers are
// "this pin cannot be honoured" paths — a Start that failed with it, and
// the launcher's --reset-transport-port after the host proved it
// unreachable — so a missing file is success, and a removal we could not
// perform is logged rather than fatal: the boot that follows is still
// correct, it just isn't guaranteed to churn the origin only once.
func clearTransportPort(dir, reason string) {
	path := filepath.Join(dir, transportPortFileName)
	previous := loadTransportPort(path)
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("transport-port: clear %s (%s): %v", path, reason, err)
		}
		return
	}
	log.Printf("transport-port: cleared pinned port %d (%s); this boot adopts a fresh one", previous, reason)
}

// loadTransportPort reads the pinned port, returning 0 for "none" —
// missing file, unreadable file, garbage JSON, or a value outside the
// range a TCP listener could ever have come from. Every one of those is
// treated as absent and replaced on the next adopt, because the file is
// a cache of a previous bind, not user configuration.
func loadTransportPort(path string) int {
	var state transportPortState
	found, err := atomicfile.ReadJSON(path, &state)
	if err != nil {
		log.Printf("transport-port: read %s: %v (falling back to an ephemeral port)", path, err)
		return 0
	}
	if !found {
		return 0
	}
	if state.Port < 1 || state.Port > 65535 {
		log.Printf("transport-port: %s holds out-of-range port %d; ignoring it", path, state.Port)
		return 0
	}
	return state.Port
}

// storeTransportPort persists the bound port for the next boot.
//
// Security posture: the bootstrap token and the Host/Origin checks are
// the access controls on this listener — port obscurity never was one,
// so pinning the port costs nothing (spec §6.0).
func storeTransportPort(dir string, port int) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("transport-port: mkdir %s: %v (port will not be stable across launches)", dir, err)
		return
	}
	path := filepath.Join(dir, transportPortFileName)
	if err := atomicfile.WriteJSON(path, transportPortState{Port: port}); err != nil {
		log.Printf("transport-port: write %s: %v (port will not be stable across launches)", path, err)
	}
}
