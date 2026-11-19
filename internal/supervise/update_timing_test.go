package supervise

import (
	"errors"
	"fmt"
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
