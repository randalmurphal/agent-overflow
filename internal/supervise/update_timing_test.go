package supervise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
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
	swap.Swapping()
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
	failing.Swapping()
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
	none.Swapping()
	none.Quitting()
	none.End(nil)
}

// The framework's Restart quits the host before it returns, so the host's
// shutdown callback (Quitting) and the end of the app loop (End) can come
// before the calls that follow Restart, and any of them can repeat. In
// every order each phase that began is logged once, timed from its first
// begin to the transition that ended it, the open one ends at End, and the
// summary is the last line and lists them. Step i runs at second i.
func TestSwapTimingTransitionsInAnyOrder(t *testing.T) {
	t.Parallel()
	type step int
	const (
		swapping step = iota
		quitting
		end
	)
	for _, tc := range []struct {
		name  string
		steps []step
		want  []string
	}{
		{"in order", []step{swapping, quitting, end}, []string{"swap:1s", "shutdown:1s"}},
		{"the shutdown callback runs inside Restart", []step{swapping, quitting, end, quitting}, []string{"swap:1s", "shutdown:1s"}},
		{"the app loop ends before Restart returns", []step{swapping, end, quitting}, []string{"swap:1s"}},
		{"each twice", []step{swapping, swapping, quitting, quitting, end, end, quitting, swapping}, []string{"swap:2s", "shutdown:2s"}},
		{"a shutdown before the swap", []step{quitting, swapping, end}, []string{"shutdown:2s"}},
		{"an end before anything", []step{end, swapping, quitting}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged []string
			clock := time.Unix(100, 0)
			swap := NewSwapTiming(func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}, "2.0.0", func() time.Time { return clock })
			for i, s := range tc.steps {
				clock = time.Unix(100+int64(i), 0)
				switch s {
				case swapping:
					swap.Swapping()
				case quitting:
					swap.Quitting()
				case end:
					swap.End(nil)
				}
			}
			wantSwapOrder(t, logged, tc.want)
		})
	}
}

// Under -race: the shutdown callback, the app loop's end, the watchdog and
// the code after Restart all run at once.
func TestSwapTimingTransitionsConcurrently(t *testing.T) {
	t.Parallel()
	for range 50 {
		var mu sync.Mutex
		var logged []string
		swap := NewSwapTiming(func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, fmt.Sprintf(format, args...))
		}, "2.0.0", nil)
		swap.Swapping()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					swap.Quitting()
				} else {
					swap.End(nil)
				}
			}()
		}
		wg.Wait()
		mu.Lock()
		got := logged
		mu.Unlock()
		want := []string{"swap"}
		if len(got) == 3 {
			want = append(want, "shutdown")
		}
		wantSwapOrder(t, got, want)
	}
}

// wantSwapOrder fails unless logged is one line for each phase of want, in
// order, then one summary listing them. An entry name:duration also wants
// that duration.
func wantSwapOrder(t *testing.T, logged, want []string) {
	t.Helper()
	text := strings.Join(logged, "\n")
	if len(logged) != len(want)+1 {
		t.Fatalf("logged:\n%s\nwant %q, then the summary", text, want)
	}
	names := make([]string, len(want))
	for i, entry := range want {
		name, took, timed := strings.Cut(entry, ":")
		names[i] = name
		m := phaseLineRE.FindStringSubmatch(logged[i])
		if m == nil || m[1] != name || timed && m[2] != took {
			t.Fatalf("line %d of\n%s\nwant phase %s", i, text, entry)
		}
	}
	want = names
	if !summaryLineRE.MatchString(logged[len(want)]) {
		t.Fatalf("logged:\n%s\nwant the summary last", text)
	}
	if len(want) == 0 {
		if !strings.Contains(logged[0], " phases= ") {
			t.Fatalf("logged:\n%s\nwant a summary listing no phases", text)
		}
		return
	}
	p := parsePhaseLines(t, logged)
	p.wantSummary(t, want...)
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

// A restore times its durable marker I/O: writing the marker before the
// copy (restore.mark, RestoreSnapshot only) and clearing it with the
// root's sync after the record (restore.clear). A marker that cannot be
// written or cleared fails its own phase, and the restore returns the same
// error as before.
func TestRestoreTimesTheMarker(t *testing.T) {
	t.Parallel()
	restored := []string{"restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.record", "restore.clear"}
	for _, tc := range []struct {
		name   string
		resume bool
		// breakWrite occupies the marker's path before the restore;
		// breakClear occupies it once the copy is done.
		breakWrite, breakClear bool
		want                   []string
		failed, wantErr        string
	}{
		{name: "restored", want: restored},
		{name: "the marker cannot be written", breakWrite: true,
			want: []string{"restore.mark"}, failed: "restore.mark", wantErr: "supervise: write restore marker: "},
		{name: "the marker cannot be cleared", breakClear: true,
			want: restored, failed: "restore.clear", wantErr: "supervise: clear restore marker: "},
		{name: "resumed", resume: true, want: restored[1:]},
		{name: "resumed, the marker cannot be cleared", resume: true, breakClear: true,
			want: restored[1:], failed: "restore.clear", wantErr: "supervise: clear restore marker: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			layout := appLayout(t, dataDir)
			writeFile(t, filepath.Join(dataDir, "agent-overflow.db"), "database")
			if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{UpdateID: "u1"}); err != nil {
				t.Fatalf("TakeSnapshot: %v", err)
			}
			if err := RecordAttempt(layout, dataDir, 1, false); err != nil {
				t.Fatal(err)
			}
			// occupy puts a directory with an entry at the marker's path,
			// which neither the marker's rename nor its removal can replace.
			occupy := func() {
				if err := os.Remove(layout.MarkerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Error(err)
				}
				writeFile(t, filepath.Join(layout.MarkerPath(), "occupied"), "")
			}
			if tc.resume {
				if err := atomicfile.WriteJSON(layout.MarkerPath(), RestoreMarker{UpdateID: "u1", DataDir: dataDir}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.breakWrite {
				occupy()
			}
			var logged []string
			opts := RestoreOptions{
				Phases: NewPhaseLog(func(format string, args ...any) {
					logged = append(logged, fmt.Sprintf(format, args...))
				}, "supervise: update u1", nil),
				Progress: func(copied, total int64) {
					if tc.breakClear && copied == total {
						occupy()
					}
				},
			}
			var err error
			if tc.resume {
				_, _, err = ResumeRestore(layout, opts)
			} else {
				err = RestoreSnapshot(layout, dataDir, "u1", "the trial failed", time.Now(), opts)
			}
			var ended []string
			for _, line := range logged {
				if m := phaseLineRE.FindStringSubmatch(line); m != nil {
					ended = append(ended, m[1])
				}
			}
			if strings.Join(ended, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("phases ended %q, want %q:\n%s", ended, tc.want, strings.Join(logged, "\n"))
			}
			p := parsePhaseLines(t, logged)
			for name, n := range p.failed {
				if name != tc.failed || n != 1 {
					t.Errorf("phase %s failed %d times, want only %q once:\n%s", name, n, tc.failed, strings.Join(logged, "\n"))
				}
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("restore = %v", err)
				}
				return
			}
			if p.failed[tc.failed] != 1 {
				t.Errorf("%s did not fail:\n%s", tc.failed, strings.Join(logged, "\n"))
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("restore = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
