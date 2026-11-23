package orphanreaper

import (
	"errors"
	"fmt"
	"log"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcInfo is the per-process introspection the sweep needs, so tests can
// inject deterministic state instead of reading the real process table.
// SystemProcInfo is the production implementation.
type ProcInfo interface {
	// Identify reads pid's identity, as Identify does; an error wraps
	// ErrNoProcess when pid names no process.
	Identify(pid int) (Identity, error)
	// PPID returns pid's current parent.
	PPID(pid int) (int, error)
	// LegacyStartMillis returns gopsutil's start time for pid, in ms since
	// the epoch, which is what earlier versions recorded as CreateUnix. It
	// exists only for those records.
	LegacyStartMillis(pid int) (int64, error)
}

// Sweep reaps process groups left orphaned by a previous app run, then
// clears the registry. Call it once at startup, before any new session
// can register, so the load→kill→clear sequence can't race a fresh Add.
func Sweep(reg *Registry, info ProcInfo, grace time.Duration) error {
	return sweepWith(reg, info, killGroup, log.Printf, grace)
}

func sweepWith(reg *Registry, info ProcInfo, kill func(pgid int, sig syscall.Signal) error, logf func(format string, args ...any), grace time.Duration) error {
	recs, err := reg.Load()
	if err != nil {
		return err
	}
	var toKill []Record
	for _, rec := range recs {
		v := judge(rec, info)
		if v.reap {
			toKill = append(toKill, rec)
			continue
		}
		why := v.why
		if v.leaderGone {
			why = leaderGone(rec, kill)
		}
		logf("orphanreaper: sweep left group %d (%s): %s", rec.PGID, rec.Owner, why)
	}
	if len(toKill) > 0 {
		signalAll(toKill, syscall.SIGTERM, kill, logf)
		time.Sleep(grace)
		signalAll(toKill, syscall.SIGKILL, kill, logf)
		logf("orphanreaper: swept %d orphaned process group(s) from a previous run", len(toKill))
	}
	// Clear the registry: every recorded entry belongs to a previous run.
	// Cross-instance safety rests on judge's parent check, not the
	// single-instance lock: a still-live sibling instance's groups are
	// still its children, so they're skipped rather than killed.
	return reg.Clear()
}

// signalAll sends sig to each group. A group already gone is the outcome
// the signal wanted; any other failure is logged.
func signalAll(recs []Record, sig syscall.Signal, kill func(pgid int, sig syscall.Signal) error, logf func(format string, args ...any)) {
	for _, rec := range recs {
		if err := kill(rec.PGID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			logf("orphanreaper: sweep could not send %v to group %d (%s): %v", sig, rec.PGID, rec.Owner, err)
		}
	}
}

// leaderGone says why a group whose leader is gone is left, and whether
// anything in the group still runs: the sweep identifies a group by its
// leader, so it cannot reap one that outlived its leader.
func leaderGone(rec Record, kill func(pgid int, sig syscall.Signal) error) string {
	err := kill(rec.PGID, 0)
	switch {
	case err == nil:
		return fmt.Sprintf("its leader %d is gone but the group still has members", rec.PID)
	case errors.Is(err, syscall.EPERM):
		return fmt.Sprintf("its leader %d is gone but the group still has members this user cannot signal", rec.PID)
	case errors.Is(err, syscall.ESRCH):
		return fmt.Sprintf("its leader %d is gone and the group is empty", rec.PID)
	default:
		return fmt.Sprintf("its leader %d is gone; probing the group failed: %v", rec.PID, err)
	}
}

// verdict is the sweep's decision for one record.
type verdict struct {
	reap bool
	// leaderGone is a recorded leader that no longer runs.
	leaderGone bool
	// why the group is left, when it is not reaped and its leader runs.
	why string
}

// judge reaps a recorded group only when its leader still runs, is the
// process that was recorded (so a recycled pid is never mistaken for it),
// and is no longer the child of the app that started it. A record that
// names no identity is never reaped.
func judge(rec Record, info ProcInfo) verdict {
	if rec.Identity == nil && rec.CreateUnix <= 0 {
		return verdict{why: "the record names no process identity"}
	}
	id, err := info.Identify(rec.PID)
	if errors.Is(err, ErrNoProcess) {
		return verdict{leaderGone: true}
	}
	if err != nil {
		return verdict{why: fmt.Sprintf("reading its leader failed: %v", err)}
	}
	if rec.Identity != nil {
		if id != *rec.Identity {
			return verdict{why: fmt.Sprintf("pid %d is now another process: recorded %+v, found %+v", rec.PID, *rec.Identity, id)}
		}
	} else {
		ms, err := info.LegacyStartMillis(rec.PID)
		if err != nil {
			return verdict{why: fmt.Sprintf("reading its leader's start time failed: %v", err)}
		}
		if ms != rec.CreateUnix {
			return verdict{why: fmt.Sprintf("pid %d is now another process: recorded start %d ms, found %d ms", rec.PID, rec.CreateUnix, ms)}
		}
	}
	ppid, err := info.PPID(rec.PID)
	if err != nil {
		return verdict{why: fmt.Sprintf("reading its leader's parent failed: %v", err)}
	}
	if rec.ParentPID > 0 {
		if ppid == rec.ParentPID {
			return verdict{why: fmt.Sprintf("its leader is still the child of the recording process %d", ppid)}
		}
		return verdict{reap: true}
	}
	if ppid != 1 {
		return verdict{why: fmt.Sprintf("its leader's parent is %d, not init", ppid)}
	}
	return verdict{reap: true}
}

// SystemProcInfo is the production ProcInfo.
func SystemProcInfo() ProcInfo { return systemProc{} }

type systemProc struct{}

func (systemProc) Identify(pid int) (Identity, error) { return Identify(pid) }

func (systemProc) PPID(pid int) (int, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	ppid, err := p.Ppid()
	if err != nil {
		return 0, err
	}
	return int(ppid), nil
}

func (systemProc) LegacyStartMillis(pid int) (int64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	return p.CreateTime()
}
