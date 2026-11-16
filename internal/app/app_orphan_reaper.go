package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"
)

// orphanReaperSweepGrace is the SIGTERM→SIGKILL window the startup sweep
// waits when tearing down orphans left by a previous run. Only paid when
// orphans are actually found.
const orphanReaperSweepGrace = 2 * time.Second

// startOrphanReaper guards against the process groups the app starts
// outliving an ungraceful app death: every group procutil.StartGroup starts,
// the headless browser and provider sessions, each through App.groupReaper.
// Windows runs its backend in WSL, so this is a no-op there.
//
// Distinct from the idle-session reaper in app_session_reaper.go, which
// closes live in-process sessions that have gone idle: that one runs
// while the app is healthy; this one defends against the app itself dying. Two layers: first sweep any orphans a previous run left
// behind (before any new session can register, so the load→kill→clear
// can't race a fresh Add), then start the live sidecar that kills watched
// groups the instant this process dies. A missing sidecar is a startup error.
func (a *App) startOrphanReaper(dbDir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	reg := orphanreaper.NewRegistry(filepath.Join(dbDir, "orphan-registry.json"))
	a.orphanRegistry = reg

	if err := orphanreaper.Sweep(reg, orphanreaper.GopsutilProcInfo(), orphanReaperSweepGrace); err != nil {
		return fmt.Errorf("startup sweep: %w", err)
	}

	client, err := orphanreaper.Spawn()
	if err != nil {
		return fmt.Errorf("start sidecar: %w", err)
	}
	a.orphanReaper = client
	log.Printf("orphanreaper: sidecar active")
	return nil
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

// appGroupReaper is the orphan reaper as procutil.StartGroup uses it. owner
// names, in the registry, what starts the groups.
type appGroupReaper struct {
	app   *App
	owner string
}

func (r appGroupReaper) Watch(pgid int)   { r.app.watchProcessGroup(pgid, r.owner) }
func (r appGroupReaper) Release(pgid int) { r.app.releaseProcessGroup(pgid) }

// groupReaper returns the reaper for the process groups one kind of command
// starts.
func (a *App) groupReaper(owner string) procutil.Reaper {
	return appGroupReaper{app: a, owner: owner}
}

// watchProcessGroup registers a freshly started process group with the
// sidecar and the durable registry. pgid is the leader's PID (its own group
// leader), and this process is its parent. owner names what started it. No-op
// when the reaper is inactive (Windows, tests) or the process never started.
// The registry Add and sidecar Watch are independent backstops, so a failure
// of one still leaves the other covering the group. The sidecar hears first:
// the Add syncs a file, and until the sidecar holds the group a death of this
// process leaves the group running until the next launch.
func (a *App) watchProcessGroup(pgid int, owner string) {
	if pgid <= 1 {
		return
	}
	a.orphanReaper.Watch(pgid)
	if a.orphanRegistry != nil {
		rec := orphanreaper.Record{
			Owner:      owner,
			PID:        pgid,
			PGID:       pgid,
			CreateUnix: orphanreaper.CaptureCreateUnix(pgid),
			ParentPID:  os.Getpid(),
		}
		if err := a.orphanRegistry.Add(rec); err != nil {
			log.Printf("orphanreaper: registry add pgid=%d: %v", pgid, err)
		}
	}
}

// releaseProcessGroup stops tracking a process group that is gone: a session
// torn down cleanly, a command whose group ended. Callers must only
// invoke this once the group is known to be gone. If a Close is abandoned
// (e.g. shutdown timeout) or a kill had no effect, the processes may still be
// alive, and keeping the watch is what guarantees they are reaped should the
// app then die. No-op when the reaper is inactive.
func (a *App) releaseProcessGroup(pgid int) {
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
