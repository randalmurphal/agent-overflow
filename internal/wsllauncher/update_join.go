package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// Joining an update in flight (docs/specs/app-update.md, Windows step 4).
// The launcher that applies an update holds no single-instance identity, so
// a launch can start while it runs, including in the gap before it starts
// applying. The launcher that started it names it in the record first
// (StartApplier). A launch that finds the named applier running joins: it
// shows the progress the applier publishes beside the record and waits for
// the applier to exit before it reconciles. The applier hides its window
// while a joiner runs (WatchJoiner) and leaves the relaunch to it.

// UpdateJoinPoll is how often a joiner reads the applier's progress and
// liveness, how often the applier looks for a joiner, and the most often
// the applier publishes its progress.
const UpdateJoinPoll = 500 * time.Millisecond

// UpdateProgressPath is where the applier of recordPath's update publishes
// its latest progress.
func UpdateProgressPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".progress.json"
}

// UpdateJoinerPath is where the launcher joined to recordPath's update names
// itself.
func UpdateJoinerPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".joiner.json"
}

// StartApplier starts cmd, the launcher that applies update id, and names it
// as the update's applier in recordPath's record before returning, so no
// launch after this launcher exits finds the update without it. A launcher
// that cannot be named is killed and reaped, and the error returned. On
// success the caller owns cmd.Process.
func StartApplier(cmd *exec.Cmd, recordPath, id string) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// cmd holds the process handle, so the id cannot name another process
	// while it is read.
	applier, err := supervise.ProcessRefOf(cmd.Process.Pid)
	if err == nil {
		err = recordApplier(recordPath, id, applier)
	}
	if err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("wsllauncher: name the launcher applying update %s: %w (and stop it: %v)", id, err, killErr)
		}
		_ = cmd.Wait() // killed: its exit status is the kill
		return fmt.Errorf("wsllauncher: name the launcher applying update %s: %w", id, err)
	}
	return nil
}

func recordApplier(recordPath, id string, applier supervise.ProcessRef) error {
	record, found, err := supervise.LoadLauncherRecord(recordPath)
	if err != nil {
		return err
	}
	if !found || record.Update.ID != id {
		return fmt.Errorf("there is no record of update %s", id)
	}
	record.Applier = &applier
	return supervise.SaveLauncherRecord(recordPath, record)
}

// ProgressPublisher publishes an applier's progress for a joiner: the latest
// report, written at most once per interval. Report never blocks.
//
// Each write replaces the file by rename (atomicfile), so a joiner reads the
// previous report or the next, never part of one. Windows refuses the
// rename while a joiner has the file open; the refused report is written
// again at the next interval unless a newer one replaced it, so the file
// always ends at the latest report.
type ProgressPublisher struct {
	path     string
	interval time.Duration
	write    func(path string, v any) error
	logf     func(string, ...any)

	mu      sync.Mutex
	pending *startupprogress.Progress
	wake    chan struct{}
	closing chan struct{}
	done    chan struct{}
}

// NewProgressPublisher publishes to path. The first failed write is logged;
// progress is not durable state.
func NewProgressPublisher(path string, interval time.Duration, logf func(string, ...any)) *ProgressPublisher {
	return newProgressPublisher(path, interval, atomicfile.WriteJSON, logf)
}

func newProgressPublisher(path string, interval time.Duration, write func(string, any) error, logf func(string, ...any)) *ProgressPublisher {
	p := &ProgressPublisher{
		path: path, interval: interval, write: write, logf: logf,
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go p.run()
	return p
}

// Report publishes progress in place of any report not yet written.
func (p *ProgressPublisher) Report(progress startupprogress.Progress) {
	p.mu.Lock()
	p.pending = &progress
	p.mu.Unlock()
	p.signal()
}

func (p *ProgressPublisher) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *ProgressPublisher) run() {
	defer close(p.done)
	logged := false
	for {
		select {
		case <-p.wake:
		case <-p.closing:
			return
		}
		p.mu.Lock()
		next := p.pending
		p.pending = nil
		p.mu.Unlock()
		if next == nil {
			continue
		}
		if err := p.write(p.path, *next); err != nil {
			if !logged {
				p.logf("updater: publish the update's progress for a joining launch: %v", err)
				logged = true
			}
			p.mu.Lock()
			if p.pending == nil {
				p.pending = next
			}
			p.mu.Unlock()
			p.signal()
		}
		t := time.NewTimer(p.interval)
		select {
		case <-t.C:
		case <-p.closing:
			t.Stop()
			return
		}
	}
}

// Close stops publishing without waiting out the interval and removes the
// file.
func (p *ProgressPublisher) Close() error {
	close(p.closing)
	<-p.done
	if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("wsllauncher: remove the published update progress: %w", err)
	}
	return nil
}

// JoinerRunning reports whether a launcher joined to recordPath's update
// runs.
func JoinerRunning(recordPath string) (bool, error) {
	var joiner supervise.ProcessRef
	found, err := atomicfile.ReadJSON(UpdateJoinerPath(recordPath), &joiner)
	if err != nil || !found {
		return false, err
	}
	return joiner.Running()
}

// WatchJoiner looks for a launcher joined to recordPath's update every poll
// and calls joined while one runs, until joined returns true or ctx ends.
// A failed look is logged once and the watch goes on.
func WatchJoiner(ctx context.Context, recordPath string, poll time.Duration, joined func() (done bool), logf func(string, ...any)) {
	failed := false
	for {
		running, err := JoinerRunning(recordPath)
		switch {
		case err != nil:
			if !failed {
				logf("updater: look for a launch joined to the update: %v", err)
			}
			failed = true
		case running && joined():
			return
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Join waits for the launcher applying record's update to exit, relaying
// the progress it publishes to s.Progress, with s.Self named as the joiner
// so the applier yields its window and relaunch. It then decides as
// Reconcile does for a launcher whose embedded payload digest is
// fingerprint, except that a committed update this launcher is not the
// target of is ReconcileRelaunch: the commit may have replaced the install
// path with the launcher this process predates.
func (s UpdateSequence) Join(ctx context.Context, record supervise.LauncherRecord, fingerprint string) (ReconcileDecision, error) {
	if err := s.waitForApplier(ctx, record); err != nil {
		return ReconcileDecision{}, err
	}
	after, found, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return ReconcileDecision{}, err
	}
	if found && after.Update.State == supervise.UpdateCommitted && fingerprint != after.TargetFingerprint {
		return ReconcileDecision{Action: ReconcileRelaunch, Record: after}, nil
	}
	return s.Reconcile(ctx, fingerprint)
}

func (s UpdateSequence) waitForApplier(ctx context.Context, record supervise.LauncherRecord) error {
	id := record.Update.ID
	if record.Applier == nil {
		return fmt.Errorf("wsllauncher: update %s names no launcher to join", id)
	}
	if s.Self.PID <= 0 || s.Self.Start == "" {
		return fmt.Errorf("wsllauncher: join update %s: this launcher is not named", id)
	}
	joinerPath := UpdateJoinerPath(s.RecordPath)
	if err := atomicfile.WriteJSON(joinerPath, s.Self); err != nil {
		return fmt.Errorf("wsllauncher: join update %s: %w", id, err)
	}
	defer func() {
		if err := os.Remove(joinerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.logf("updater: leave update %s: %v", id, err)
		}
	}()
	s.logf("updater: joining update %s, applied by launcher pid %d", id, record.Applier.PID)

	poll := s.JoinPoll
	if poll <= 0 {
		poll = UpdateJoinPoll
	}
	progressPath := UpdateProgressPath(s.RecordPath)
	var last *startupprogress.Progress
	readFailed := false
	for {
		running, err := record.Applier.Running()
		if err != nil {
			return fmt.Errorf("wsllauncher: check the launcher applying update %s: %w", id, err)
		}
		if !running {
			s.logf("updater: the launcher applying update %s exited", id)
			return nil
		}
		var p startupprogress.Progress
		found, err := atomicfile.ReadJSON(progressPath, &p)
		switch {
		case err != nil:
			if !readFailed {
				s.logf("updater: read the progress of update %s: %v", id, err)
			}
			readFailed = true
		case found && (last == nil || p != *last):
			last = &p
			if s.Progress != nil {
				s.Progress(p)
			}
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
