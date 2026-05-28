package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agent-overflow/internal/orphanreaper"
)

// orphanReaperSweepGrace is the SIGTERM→SIGKILL window the startup sweep
// waits when tearing down orphans left by a previous run. Only paid when
// orphans are actually found.
const orphanReaperSweepGrace = 2 * time.Second

// startOrphanReaper is the macOS-only guard against provider subprocesses
// outliving an ungraceful app death. Linux relies on Pdeathsig
// (process_linux.go) and Windows on the launcher's Job Object, so this is
// a no-op there.
//
// Distinct from the idle-session reaper in app_session_reaper.go, which
// closes live in-process sessions that have gone idle: that one runs
// while the app is healthy; this one defends against the app itself dying. Two layers: first sweep any orphans a previous run left
// behind (before any new session can register, so the load→kill→clear
// can't race a fresh Add), then start the live sidecar that kills watched
// groups the instant this process dies. Failures degrade rather than
// block startup — without the sidecar, the next launch's sweep is still a
// backstop.
func (a *App) startOrphanReaper(dbDir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	reg := orphanreaper.NewRegistry(filepath.Join(dbDir, "orphan-registry.json"))
	a.orphanRegistry = reg

	if err := orphanreaper.Sweep(reg, orphanreaper.GopsutilProcInfo(), orphanReaperSweepGrace); err != nil {
		log.Printf("orphanreaper: startup sweep: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		log.Printf("orphanreaper: cannot resolve executable, skipping sidecar: %v", err)
		return
	}
	client, err := orphanreaper.Spawn(exe)
	if err != nil {
		log.Printf("orphanreaper: start sidecar: %v", err)
		return
	}
	a.orphanReaper = client
	log.Printf("orphanreaper: sidecar active")
}

// stopOrphanReaper closes the sidecar at shutdown. Closing the control
// pipe sends EOF, so the sidecar reaps any still-watched groups (none, if
// every session was released first) and exits. Nil-safe.
func (a *App) stopOrphanReaper() {
	if a.orphanReaper == nil {
		return
	}
	if err := a.orphanReaper.Close(); err != nil {
		log.Printf("orphanreaper: close sidecar: %v", err)
	}
}

// watchSessionProcess registers a freshly-spawned provider process group
// with the sidecar and the durable registry. pgid is the provider PID
// (its own group leader via Setpgid). No-op when the reaper is inactive
// (non-macOS, tests) or the process never started. The registry Add and
// sidecar Watch are independent backstops, so a failure of one still
// leaves the other covering the group.
func (a *App) watchSessionProcess(pgid int, uuid string) {
	if pgid <= 1 {
		return
	}
	if a.orphanRegistry != nil {
		rec := orphanreaper.Record{
			UUID:       uuid,
			PID:        pgid,
			PGID:       pgid,
			CreateUnix: orphanreaper.CaptureCreateUnix(pgid),
		}
		if err := a.orphanRegistry.Add(rec); err != nil {
			log.Printf("orphanreaper: registry add pgid=%d: %v", pgid, err)
		}
	}
	a.orphanReaper.Watch(pgid)
}

// releaseSessionProcess stops tracking a process group whose session was
// torn down cleanly. Callers must only invoke this after a *successful*
// Close — if a Close is abandoned (e.g. shutdown timeout) the subprocess
// may still be alive, and keeping the watch is what guarantees it's
// reaped should the app then die. No-op when the reaper is inactive.
func (a *App) releaseSessionProcess(pgid int) {
	if pgid <= 1 {
		return
	}
	if a.orphanRegistry != nil {
		if err := a.orphanRegistry.Remove(pgid); err != nil {
			log.Printf("orphanreaper: registry remove pgid=%d: %v", pgid, err)
		}
	}
	a.orphanReaper.Release(pgid)
}
