package supervise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	phaseLineRE   = regexp.MustCompile(`: phase=(\S+) duration=(\S+)`)
	skippedLineRE = regexp.MustCompile(`: phase=(\S+) skipped: `)
	summaryLineRE = regexp.MustCompile(`: summary total=(\S+) phases=(\S*)`)
)

// phaseLines is what an update's log says about its phases.
type phaseLines struct {
	// timed counts the lines of each phase that ran, failed counts those
	// that failed, and skipped those that did not run.
	timed, failed, skipped map[string]int
	// summaries holds each summary line's phase entries, names with their
	// outcome: "trial.1", "trial.1(failed)" or "snapshot:skipped".
	summaries [][]string
}

// parsePhaseLines reads logged, failing the test on any duration that does
// not parse.
func parsePhaseLines(t *testing.T, logged []string) phaseLines {
	t.Helper()
	p := phaseLines{timed: map[string]int{}, failed: map[string]int{}, skipped: map[string]int{}}
	for _, line := range logged {
		if m := phaseLineRE.FindStringSubmatch(line); m != nil {
			if _, err := time.ParseDuration(m[2]); err != nil {
				t.Errorf("phase line %q: duration: %v", line, err)
			}
			p.timed[m[1]]++
			if strings.Contains(line, " failed: ") {
				p.failed[m[1]]++
			}
			continue
		}
		if m := skippedLineRE.FindStringSubmatch(line); m != nil {
			p.skipped[m[1]]++
			continue
		}
		m := summaryLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, err := time.ParseDuration(m[1]); err != nil {
			t.Errorf("summary %q: total: %v", line, err)
		}
		var entries []string
		for _, entry := range strings.Split(m[2], ",") {
			name, value, ok := strings.Cut(entry, ":")
			if !ok {
				t.Errorf("summary %q: entry %q has no value", line, entry)
				continue
			}
			if value == "skipped" {
				entries = append(entries, name+":skipped")
				continue
			}
			failed := strings.HasSuffix(value, "(failed)")
			if _, err := time.ParseDuration(strings.TrimSuffix(value, "(failed)")); err != nil {
				t.Errorf("summary %q: entry %q: %v", line, entry, err)
			}
			if failed {
				name += "(failed)"
			}
			entries = append(entries, name)
		}
		p.summaries = append(p.summaries, entries)
	}
	return p
}

// wantOnce fails unless each phase has exactly one line with a duration.
func (p phaseLines) wantOnce(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if n := p.timed[name]; n != 1 {
			t.Errorf("phase %s logged %d times, want once", name, n)
		}
	}
}

// wantNone fails when any of names was logged as having run.
func (p phaseLines) wantNone(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if n := p.timed[name]; n != 0 {
			t.Errorf("phase %s logged %d times, want none: it did not run", name, n)
		}
	}
}

// wantSummary fails unless there is exactly one summary, listing want.
func (p phaseLines) wantSummary(t *testing.T, want ...string) {
	t.Helper()
	if len(p.summaries) != 1 {
		t.Fatalf("%d summary lines, want one: %q", len(p.summaries), p.summaries)
	}
	if got := strings.Join(p.summaries[0], ","); got != strings.Join(want, ",") {
		t.Fatalf("summary phases = %s\nwant %s", got, strings.Join(want, ","))
	}
}

func TestPhaseLogWritesOneLinePerPhaseAndASummary(t *testing.T) {
	t.Parallel()
	clock := time.Unix(100, 0)
	var logged []string
	l := NewPhaseLog(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}, "updater: update u1", func() time.Time { return clock })

	copyPhase := l.Begin("snapshot.copy")
	clock = clock.Add(1500 * time.Millisecond)
	copyPhase.End(nil, "file", "agent-overflow.db", "bytes", 42, "cloned", false)
	l.Skip("publish", "a database upgrade installs nothing")
	trial := l.Begin("trial.1")
	clock = clock.Add(2 * time.Second)
	trial.Waited(errors.New("exit status 3"))
	settle := l.Begin("settle")
	settle.End(errors.New("disk full"))
	l.Summary(4*time.Second, "outcome", "rolled-back")

	want := []string{
		"updater: update u1: phase=snapshot.copy duration=1.5s file=agent-overflow.db bytes=42 cloned=false",
		"updater: update u1: phase=publish skipped: a database upgrade installs nothing",
		"updater: update u1: phase=settle duration=0s failed: disk full",
		"updater: update u1: summary total=4s phases=snapshot.copy:1.5s,publish:skipped,trial.1:2s(failed),settle:0s(failed) outcome=rolled-back",
	}
	if strings.Join(logged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged:\n%s\nwant:\n%s", strings.Join(logged, "\n"), strings.Join(want, "\n"))
	}
	p := parsePhaseLines(t, logged)
	p.wantSummary(t, "snapshot.copy", "publish:skipped", "trial.1(failed)", "settle(failed)")

	var none *PhaseLog
	none.Begin("x").End(nil)
	none.Skip("x", "y")
	none.Summary(time.Second)
}

// An update without a trial logs the branch it took, never a failed
// handoff: a handoff that found the target cannot run the trial is logged
// on the swap with branch=swap, and on the trial's log not at all. Then the
// steps, the swap's start, the shutdown and one summary; a second end
// writes nothing. A swap that fails ends with its error and no shutdown.
func TestSwapTimingLogsTheBranchItsPhasesAndASummary(t *testing.T) {
	t.Parallel()
	clock := time.Unix(100, 0)
	now := func() time.Time { return clock }
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	swap := NewSwapTiming(logf, "2.0.0", now)
	handing := NewPhaseLog(logf, "updater: update u1", now).Begin("handoff")
	clock = clock.Add(3 * time.Second)
	swap.Declined(handing)
	verifying := swap.Phases().Begin("verify")
	clock = clock.Add(time.Second)
	verifying.End(nil)
	swapping := swap.Phases().Begin("swap")
	swapping.End(nil)
	swap.Quitting()
	clock = clock.Add(2 * time.Second)
	swap.End(nil)
	swap.End(errors.New("a second end"))
	swap.Quitting()

	want := []string{
		"updater: swap to 2.0.0: phase=handoff duration=3s branch=swap",
		"updater: swap to 2.0.0: phase=verify duration=1s",
		"updater: swap to 2.0.0: phase=swap duration=0s",
		"updater: swap to 2.0.0: phase=shutdown duration=2s",
		"updater: swap to 2.0.0: summary total=6s phases=handoff:3s,verify:1s,swap:0s,shutdown:2s outcome=swapped",
	}
	if strings.Join(logged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged:\n%s\nwant:\n%s", strings.Join(logged, "\n"), strings.Join(want, "\n"))
	}

	logged = nil
	failing := NewSwapTiming(logf, "", now)
	failing.Phases().Skip("handoff", "the target is the running version")
	failing.Phases().Begin("swap").End(errors.New("spawn helper: boom"))
	failing.End(errors.New("spawn helper: boom"))
	want = []string{
		"updater: swap: phase=handoff skipped: the target is the running version",
		"updater: swap: phase=swap duration=0s failed: spawn helper: boom",
		"updater: swap: summary total=0s phases=handoff:skipped,swap:0s(failed) outcome=error",
	}
	if strings.Join(logged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged:\n%s\nwant:\n%s", strings.Join(logged, "\n"), strings.Join(want, "\n"))
	}

	var none *SwapTiming
	none.Declined(handing)
	none.Phases().Begin("x").End(nil)
	none.Quitting()
	none.End(nil)
}

// A restore's sync ends before the update's attempt record is written, and
// the record is its own phase: one that cannot be written fails that phase
// and the restore with the same error as before, never the sync.
func TestRestoreTimesTheAttemptRecordApartFromTheSync(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		breakRecord bool
	}{
		{"recorded", false},
		{"the record cannot be written", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			layout := appLayout(t, dataDir)
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "database")
			if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{UpdateID: "u1"}); err != nil {
				t.Fatalf("TakeSnapshot: %v", err)
			}
			if err := RecordAttempt(layout, dataDir, 2, false); err != nil {
				t.Fatal(err)
			}
			var logged []string
			phases := NewPhaseLog(func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}, "supervise: update u1", nil)
			broken := false
			err := RestoreSnapshot(layout, dataDir, "u1", "the trial failed", time.Now(), RestoreOptions{
				Phases: phases,
				// The manifest was read before the copy; a directory in its
				// place now refuses the record's rename, and nothing else.
				Progress: func(copied, total int64) {
					if !tc.breakRecord || broken || copied < total {
						return
					}
					broken = true
					manifest := filepath.Join(layout.SnapshotDir(), snapshotManifest)
					if err := os.Remove(manifest); err != nil {
						t.Error(err)
					}
					writeFile(t, filepath.Join(manifest, "occupied"), "")
				},
			})
			p := parsePhaseLines(t, logged)
			p.wantOnce(t, "restore.remove", "restore.copy", "restore.sync", "restore.record")
			if p.failed["restore.sync"] != 0 {
				t.Errorf("the sync is logged failed:\n%s", strings.Join(logged, "\n"))
			}
			sync, record := -1, -1
			for i, line := range logged {
				switch {
				case strings.Contains(line, "phase=restore.sync "):
					sync = i
				case strings.Contains(line, "phase=restore.record "):
					record = i
				}
			}
			if sync < 0 || record < sync {
				t.Errorf("restore.sync ended at line %d and restore.record at %d; want the sync first:\n%s",
					sync, record, strings.Join(logged, "\n"))
			}
			if !tc.breakRecord {
				if err != nil || p.failed["restore.record"] != 0 {
					t.Fatalf("RestoreSnapshot = %v, failed %v", err, p.failed)
				}
				return
			}
			if !broken {
				t.Fatal("the copy reported no progress, so the record was never broken")
			}
			if err == nil || !strings.HasPrefix(err.Error(), "supervise: record the restored database for trial attempt 2: ") {
				t.Fatalf("RestoreSnapshot = %v, want the record's own error", err)
			}
			if p.failed["restore.record"] != 1 {
				t.Fatalf("failed %v, want restore.record:\n%s", p.failed, strings.Join(logged, "\n"))
			}
		})
	}
}
