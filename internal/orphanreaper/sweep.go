package orphanreaper

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"agent-overflow/internal/harness/instanceinfo"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcInfo is the per-process introspection the capture and the sweep
// need, so tests can inject deterministic state instead of reading the real
// process table. SystemProcInfo is the production implementation.
type ProcInfo interface {
	// Read reads pid's kernel process record; found is false, with a nil
	// error, when pid names no process.
	Read(pid int) (proc instanceinfo.Process, found bool, err error)
	// Boot reads the kernel's boot id; empty where start times are absolute.
	Boot() (string, error)
	// LegacyStartMillis returns gopsutil's start time for pid, in ms since
	// the epoch, which is what earlier versions recorded as create_unix.
	// It exists for those versions alone: the sweep compares it only for a
	// record an earlier version wrote, and the capture writes it for an
	// earlier version that reads a new record.
	LegacyStartMillis(pid int) (int64, error)
}

// Sweep ends process groups left orphaned by a previous app run. Call it
// once at startup, before any new session can register: it rewrites the
// registry with the records it keeps for the next launch.
func Sweep(reg *Registry, info ProcInfo, grace time.Duration) error {
	stop := func(pgid int) error { return stopGroup(pgid, grace) }
	return sweepWith(reg, info, stop, probeGroup, log.Printf)
}

func sweepWith(reg *Registry, info ProcInfo, stop, probe func(pgid int) error, logf func(format string, args ...any)) error {
	recs, err := reg.Load()
	if err != nil {
		return err
	}
	var kept, toStop []Record
	for _, rec := range recs {
		v := judge(rec, info)
		switch v.outcome {
		case reap:
			toStop = append(toStop, rec)
		case keep:
			kept = append(kept, rec)
			logf("orphanreaper: sweep kept group %d (%s) for the next launch: %s", rec.Leader, rec.Owner, v.why)
		case drop:
			why := v.why
			if v.leaderGone {
				why = leaderGone(rec, probe)
			}
			logf("orphanreaper: sweep left group %d (%s): %s", rec.Leader, rec.Owner, why)
		}
	}
	swept := 0
	for i, err := range stopAll(toStop, stop) {
		rec := toStop[i]
		switch {
		case err == nil:
			swept++
		case errors.Is(err, os.ErrProcessDone):
			logf("orphanreaper: sweep left group %d (%s): it ended before the sweep signalled it", rec.Leader, rec.Owner)
		default:
			kept = append(kept, rec)
			logf("orphanreaper: sweep kept group %d (%s) for the next launch: could not end it: %v", rec.Leader, rec.Owner, err)
		}
	}
	if swept > 0 {
		logf("orphanreaper: swept %d orphaned process group(s) from a previous run", swept)
	}
	return reg.replace(kept)
}

// stopAll stops every group at once, so the sweep waits one grace for all
// of them, and returns each group's result in order.
func stopAll(recs []Record, stop func(pgid int) error) []error {
	errs := make([]error, len(recs))
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Go(func() { errs[i] = stop(rec.Leader) })
	}
	wg.Wait()
	return errs
}

// leaderGone says why a group whose leader is gone is left, and whether
// anything in the group still runs: the sweep identifies a group by its
// leader, so it cannot reap one that outlived its leader.
func leaderGone(rec Record, probe func(pgid int) error) string {
	err := probe(rec.Leader)
	switch {
	case err == nil:
		return fmt.Sprintf("its leader %d is gone but the group still has members", rec.Leader)
	case errors.Is(err, os.ErrProcessDone):
		return fmt.Sprintf("its leader %d is gone and the group is empty", rec.Leader)
	case errors.Is(err, syscall.EPERM):
		return fmt.Sprintf("its leader %d is gone but the group still has members this user cannot signal", rec.Leader)
	default:
		return fmt.Sprintf("its leader %d is gone; probing the group failed: %v", rec.Leader, err)
	}
}

// outcome is what the sweep does with one record.
type outcome int

const (
	// drop: the group ended or the record is stale; the record goes.
	drop outcome = iota
	// reap: the group is an orphan of the recording process; the sweep
	// stops it and keeps the record if it cannot confirm the group ended.
	reap
	// keep: the sweep cannot decide now; the record stays for the next
	// launch.
	keep
)

// verdict is the sweep's decision for one record.
type verdict struct {
	outcome outcome
	// leaderGone is a drop because the recorded leader no longer runs.
	leaderGone bool
	// why the group is left or kept.
	why string
}

func keepFor(format string, args ...any) verdict {
	return verdict{outcome: keep, why: fmt.Sprintf(format, args...)}
}

func dropFor(format string, args ...any) verdict {
	return verdict{outcome: drop, why: fmt.Sprintf(format, args...)}
}

// judge reaps a recorded group only when its leader still runs, is the
// process that was recorded (so a recycled pid is never mistaken for it),
// and is no longer the child of the app that started it. The record's
// format picks the reader: an identity, or the start time an earlier
// version recorded.
func judge(rec Record, info ProcInfo) verdict {
	var ppid int
	if rec.Identity != nil {
		snap, err := observe(info, rec.Leader)
		if errors.Is(err, ErrNoProcess) {
			return verdict{outcome: drop, leaderGone: true}
		}
		if err != nil {
			return keepFor("reading its leader failed: %v", err)
		}
		if snap.Identity != *rec.Identity {
			return dropFor("pid %d now names another process, or this sweep reads from another time namespace than the recorder: recorded %+v, found %+v", rec.Leader, *rec.Identity, snap.Identity)
		}
		ppid = snap.PPID
	} else {
		proc, found, err := info.Read(rec.Leader)
		if err != nil {
			return keepFor("reading its leader failed: %v", err)
		}
		if !found {
			return verdict{outcome: drop, leaderGone: true}
		}
		ms, err := info.LegacyStartMillis(rec.Leader)
		if err != nil {
			return keepFor("reading its leader's start time failed: %v", err)
		}
		if ms != rec.CreateUnix {
			return dropFor("pid %d now names another process: recorded start %d ms, found %d ms", rec.Leader, rec.CreateUnix, ms)
		}
		ppid = proc.PPID
	}
	if rec.Parent > 0 {
		if ppid == rec.Parent {
			return keepFor("its leader is still the child of the recording process %d", ppid)
		}
		return verdict{outcome: reap}
	}
	if ppid != 1 {
		return keepFor("its leader's parent is %d, not init", ppid)
	}
	return verdict{outcome: reap}
}

// SystemProcInfo is the production ProcInfo.
func SystemProcInfo() ProcInfo { return systemProc{} }

type systemProc struct{}

func (systemProc) Read(pid int) (instanceinfo.Process, bool, error) { return readProcess(pid) }

func (systemProc) Boot() (string, error) { return bootID() }

func (systemProc) LegacyStartMillis(pid int) (int64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	return p.CreateTime()
}
