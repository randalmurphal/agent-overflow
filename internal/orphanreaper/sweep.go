package orphanreaper

import (
	"log"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcInfo abstracts the per-process introspection the sweep needs so
// tests can inject deterministic state instead of scanning the real
// process table. GopsutilProcInfo is the production implementation.
type ProcInfo interface {
	// Running reports whether pid refers to a live process.
	Running(pid int) bool
	// CreateUnix returns the process start time (ms since epoch); ok is
	// false when the process is gone or the value is unavailable.
	CreateUnix(pid int) (ms int64, ok bool)
	// PPID returns the current parent pid (1 == reparented to launchd/init
	// because the original parent died); ok is false if unavailable.
	PPID(pid int) (ppid int, ok bool)
}

// Sweep reaps provider groups left orphaned by a previous app run, then
// clears the registry. Call it once at startup, before any new session
// can register, so the load→kill→clear sequence can't race a fresh Add.
func Sweep(reg *Registry, info ProcInfo, grace time.Duration) error {
	return sweepWith(reg, info, killGroup, grace)
}

func sweepWith(reg *Registry, info ProcInfo, kill func(pgid int, sig syscall.Signal), grace time.Duration) error {
	recs, err := reg.Load()
	if err != nil {
		return err
	}
	var toKill []int
	for _, rec := range recs {
		if shouldReap(rec, info) {
			toKill = append(toKill, rec.PGID)
		}
	}
	if len(toKill) > 0 {
		for _, pgid := range toKill {
			kill(pgid, syscall.SIGTERM)
		}
		time.Sleep(grace)
		for _, pgid := range toKill {
			kill(pgid, syscall.SIGKILL)
		}
		log.Printf("orphanreaper: swept %d orphaned provider group(s) from a previous run", len(toKill))
	}
	// Clear the registry: every recorded entry belongs to a previous run.
	// Cross-instance safety rests on shouldReap's ppid==1 check, not the
	// single-instance lock — a still-live sibling instance's providers
	// aren't reparented to init, so they're skipped rather than killed.
	// Killed entries are gone; skipped ones are already dead or PID-reused.
	return reg.Clear()
}

// shouldReap kills a recorded group only when the leader process is still
// alive, its start time matches what we recorded (so a recycled PID is
// never mistaken for ours), and it has been reparented to init (the app
// that spawned it is gone). A record with CreateUnix==0 (start time
// couldn't be captured at spawn) falls back to the orphaned-parent check.
func shouldReap(rec Record, info ProcInfo) bool {
	if !info.Running(rec.PID) {
		return false
	}
	if rec.CreateUnix > 0 {
		if ct, ok := info.CreateUnix(rec.PID); !ok || ct != rec.CreateUnix {
			return false
		}
	}
	ppid, ok := info.PPID(rec.PID)
	if !ok || ppid != 1 {
		return false
	}
	return true
}

// GopsutilProcInfo is the production ProcInfo backed by gopsutil (already
// a dependency via internal/sysstat; no CGo).
func GopsutilProcInfo() ProcInfo { return gopsutilProc{} }

type gopsutilProc struct{}

func (gopsutilProc) Running(pid int) bool {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, err := p.IsRunning()
	return err == nil && running
}

func (gopsutilProc) CreateUnix(pid int) (int64, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	ms, err := p.CreateTime()
	if err != nil {
		return 0, false
	}
	return ms, true
}

func (gopsutilProc) PPID(pid int) (int, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	ppid, err := p.Ppid()
	if err != nil {
		return 0, false
	}
	return int(ppid), true
}

// CaptureCreateUnix returns a process's start time (ms since epoch) for
// recording in a Record at spawn, or 0 if it can't be read.
func CaptureCreateUnix(pid int) int64 {
	ms, ok := gopsutilProc{}.CreateUnix(pid)
	if !ok {
		return 0
	}
	return ms
}
