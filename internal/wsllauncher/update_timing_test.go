package wsllauncher

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/supervise"
)

var (
	launcherPhaseRE   = regexp.MustCompile(`: phase=(\S+) duration=(\S+)`)
	launcherSummaryRE = regexp.MustCompile(`: summary total=(\S+) phases=(\S*)`)
)

// captureLog makes the fixture's sequence log into the returned slice.
func (f *updateFixture) captureLog() *[]string {
	var logged []string
	f.sequence.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	return &logged
}

// launcherPhases counts the launcher's own phase lines and returns its one
// summary's entries, failing on a duration that does not parse.
func launcherPhases(t *testing.T, logged []string) (timed map[string]int, summary string) {
	t.Helper()
	timed = map[string]int{}
	summaries := 0
	for _, line := range logged {
		if m := launcherPhaseRE.FindStringSubmatch(line); m != nil {
			if _, err := time.ParseDuration(m[2]); err != nil {
				t.Errorf("phase line %q: %v", line, err)
			}
			timed[m[1]]++
			continue
		}
		m := launcherSummaryRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		summaries++
		if _, err := time.ParseDuration(m[1]); err != nil {
			t.Errorf("summary %q: total: %v", line, err)
		}
		var names []string
		for _, entry := range strings.Split(m[2], ",") {
			name, value, _ := strings.Cut(entry, ":")
			if value == "skipped" {
				names = append(names, name+":skipped")
				continue
			}
			if v, failed := strings.CutSuffix(value, "(failed)"); failed {
				value, name = v, name+"(failed)"
			}
			if _, err := time.ParseDuration(value); err != nil {
				t.Errorf("summary %q: entry %q: %v", line, entry, err)
			}
			names = append(names, name)
		}
		summary = strings.Join(names, ",")
	}
	if summaries != 1 {
		t.Fatalf("%d summary lines, want one:\n%s", summaries, strings.Join(logged, "\n"))
	}
	return timed, summary
}

// The launcher runs the snapshot, trial, restore and discard as WSL
// commands, which log their own phases; its log has its wait for each in
// the summary and a line for each phase it runs itself.
func TestApplyUpdateLogsTheLaunchersPhasesAndASummary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		setup    func(*fakeUpdateHost)
		lines    []string
		summary  string
	}{
		{"commit", 0, func(*fakeUpdateHost) {},
			[]string{"record-attempt", "settle", "publish"},
			"snapshot,record-attempt,trial.1,settle,publish"},
		{"rollback through the stable payload", 0, func(h *fakeUpdateHost) {
			h.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeFailed, "boot failed")
		}, []string{"record-attempt", "settle", "remove-staged"},
			"snapshot,record-attempt,trial.1(failed),restore,settle,discard,remove-staged"},
		{"retry", 1, func(*fakeUpdateHost) {},
			[]string{"record-attempt", "settle", "publish"},
			"snapshot:skipped,record-attempt,trial.2,settle,publish"},
		{"snapshot failed", 0, func(h *fakeUpdateHost) {
			h.fail(supervise.UpdateSnapshotCommand, errors.New("the __update-snapshot step exited without a result"))
		}, []string{"settle", "remove-staged"},
			"snapshot(failed),settle,discard,remove-staged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.save(supervise.UpdatePending, tc.attempts, false)
			tc.setup(f.host)
			logged := f.captureLog()
			if _, err := f.sequence.Apply(t.Context(), "u1"); err != nil {
				t.Fatal(err)
			}
			timed, summary := launcherPhases(t, *logged)
			for _, name := range tc.lines {
				if timed[name] != 1 {
					t.Errorf("phase %s logged %d times, want once:\n%s", name, timed[name], strings.Join(*logged, "\n"))
				}
			}
			for _, name := range []string{"snapshot", "trial.1", "trial.2", "restore", "discard"} {
				if timed[name] != 0 {
					t.Errorf("the launcher logged the %s command's phase, which the command logs where it runs", name)
				}
			}
			if summary != tc.summary {
				t.Fatalf("summary = %s\nwant %s", summary, tc.summary)
			}
		})
	}
}
