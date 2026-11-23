package orphanreaper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// fakeProc is a deterministic ProcInfo keyed by pid. A pid absent from
// procs names no process.
type fakeProc struct {
	procs   map[int]instanceinfo.Process
	readErr map[int]error
	boot    string
	bootErr error
	// legacy is what gopsutil's wall-clock-derived start time reads now.
	legacy map[int]int64
}

func (f fakeProc) Read(pid int) (instanceinfo.Process, bool, error) {
	if err := f.readErr[pid]; err != nil {
		return instanceinfo.Process{}, false, err
	}
	proc, ok := f.procs[pid]
	return proc, ok, nil
}

func (f fakeProc) Boot() (string, error) { return f.boot, f.bootErr }

func (f fakeProc) LegacyStartMillis(pid int) (int64, error) {
	v, ok := f.legacy[pid]
	if !ok {
		return 0, fmt.Errorf("no start time for %d", pid)
	}
	return v, nil
}

const (
	bootA       = "boot-a"
	leaderStart = "6299260"
	// recordedMs is the wall-clock-derived start time the capture read.
	recordedMs = int64(1_700_000_000_000)
)

// running is the kernel's record of a leader that started at leaderStart.
func running(ppid int) instanceinfo.Process {
	return instanceinfo.Process{Start: leaderStart, PPID: ppid}
}

// recorded is a group this version recorded under parent.
func recorded(owner string, pid, parent int) Record {
	return Record{Owner: owner, Leader: pid, Identity: &Identity{Boot: bootA, Start: leaderStart}, CreateUnix: recordedMs, Parent: parent}
}

// earlier is a group an earlier version recorded under parent.
func earlier(pid, parent int) Record {
	return Record{Owner: "earlier", Leader: pid, CreateUnix: recordedMs, Parent: parent}
}

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		info fakeProc
		want outcome
		gone bool
		why  string
	}{
		{
			// The wall-clock start time reads a second off what the capture
			// read, as a container's computed boot time does; the kernel's
			// values agree.
			name: "the recorded leader, whatever the wall clock says, orphaned -> reap",
			rec:  recorded("a", 10, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{10: running(1)}, boot: bootA, legacy: map[int]int64{10: recordedMs + 1000}},
			want: reap,
		},
		{
			name: "same pid, another kernel start (pid reused) -> drop",
			rec:  recorded("a", 11, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{11: {Start: "7000000", PPID: 1}}, boot: bootA, legacy: map[int]int64{11: recordedMs}},
			want: drop,
			why:  "pid 11 now names another process",
		},
		{
			name: "same pid and start in another boot -> drop",
			rec:  recorded("a", 12, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{12: running(1)}, boot: "boot-b", legacy: map[int]int64{12: recordedMs}},
			want: drop,
			why:  "pid 12 now names another process",
		},
		{
			name: "leader gone -> drop, probed",
			rec:  recorded("a", 13, 500),
			info: fakeProc{boot: bootA},
			want: drop,
			gone: true,
		},
		{
			name: "leader unreadable -> keep",
			rec:  recorded("a", 14, 500),
			info: fakeProc{readErr: map[int]error{14: errors.New("permission denied")}, boot: bootA},
			want: keep,
			why:  "reading its leader failed: permission denied",
		},
		{
			name: "boot id unreadable -> keep",
			rec:  recorded("a", 15, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{15: running(1)}, bootErr: errors.New("read boot id: denied")},
			want: keep,
			why:  "reading its leader failed: read boot id: denied",
		},
		{
			name: "still the recording parent's child -> keep",
			rec:  recorded("a", 16, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{16: running(500)}, boot: bootA},
			want: keep,
			why:  "still the child of the recording process 500",
		},
		{
			name: "reparented to a subreaper -> reap",
			rec:  recorded("a", 17, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{17: running(77)}, boot: bootA},
			want: reap,
		},
		{
			name: "no parent recorded, under init -> reap",
			rec:  recorded("a", 18, 0),
			info: fakeProc{procs: map[int]instanceinfo.Process{18: running(1)}, boot: bootA},
			want: reap,
		},
		{
			name: "no parent recorded, not under init -> keep",
			rec:  recorded("a", 19, 0),
			info: fakeProc{procs: map[int]instanceinfo.Process{19: running(4242)}, boot: bootA},
			want: keep,
			why:  "parent is 4242, not init",
		},
		// An earlier version's record is read without the boot id, which
		// these readers cannot supply.
		{
			name: "an earlier version's record whose start time matches -> reap",
			rec:  earlier(20, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{20: running(1)}, bootErr: errors.New("no boot id"), legacy: map[int]int64{20: recordedMs}},
			want: reap,
		},
		{
			name: "an earlier version's record whose start time differs -> drop",
			rec:  earlier(21, 500),
			info: fakeProc{procs: map[int]instanceinfo.Process{21: running(1)}, bootErr: errors.New("no boot id"), legacy: map[int]int64{21: recordedMs + 9000}},
			want: drop,
			why:  "recorded start 1700000000000 ms, found 1700000009000 ms",
		},
		{
			name: "an earlier version's record whose leader is gone -> drop, probed",
			rec:  earlier(22, 500),
			info: fakeProc{bootErr: errors.New("no boot id")},
			want: drop,
			gone: true,
		},
		{
			name: "an earlier version's record whose leader is unreadable -> keep",
			rec:  earlier(23, 500),
			info: fakeProc{readErr: map[int]error{23: errors.New("permission denied")}, bootErr: errors.New("no boot id")},
			want: keep,
			why:  "reading its leader failed: permission denied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judge(tc.rec, tc.info)
			if got.outcome != tc.want || got.leaderGone != tc.gone {
				t.Fatalf("judge = %+v, want outcome %d, leader gone %v", got, tc.want, tc.gone)
			}
			if !strings.Contains(got.why, tc.why) {
				t.Fatalf("judge's reason %q, want %q", got.why, tc.why)
			}
		})
	}
}

// stops records which groups the sweep stopped and answers each from
// results, one answer per sweep in order; a group with no answer left
// ends.
type stops struct {
	mu      sync.Mutex
	results map[int][]error
	stopped []int
}

func (s *stops) stop(pgid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, pgid)
	answers := s.results[pgid]
	if len(answers) == 0 {
		return nil
	}
	s.results[pgid] = answers[1:]
	return answers[0]
}

func (s *stops) of(pgid int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.stopped {
		if p == pgid {
			n++
		}
	}
	return n
}

// probes answers a group probe from members, os.ErrProcessDone when absent.
func probes(members map[int]error) func(int) error {
	return func(pgid int) error {
		if err, ok := members[pgid]; ok {
			return err
		}
		return os.ErrProcessDone
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func seed(t *testing.T, recs ...Record) *Registry {
	t.Helper()
	reg, _ := newTestRegistry(t)
	if err := reg.replace(recs); err != nil {
		t.Fatal(err)
	}
	return reg
}

func leaders(t *testing.T, reg *Registry) []int {
	t.Helper()
	recs, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, rec := range recs {
		out = append(out, rec.Leader)
	}
	return out
}

// Every group the sweep leaves or keeps is logged once, with its owner,
// pgid and why; a group whose leader is gone says whether it still has
// members. Only a group the stop confirmed ended counts as swept.
func TestSweepLogsEachGroupItLeavesOrKeeps(t *testing.T) {
	reg := seed(t,
		recorded("shell command", 10, 500),
		recorded("terminal", 11, 500),
		recorded("headless browser", 12, 500),
		recorded("codex read", 13, 500),
		recorded("mcp sign-in", 14, 500),
		recorded("provider", 15, 500),
	)
	info := fakeProc{
		procs: map[int]instanceinfo.Process{
			10: running(1), 13: {Start: "1", PPID: 1}, 14: running(1), 15: running(1),
		},
		readErr: map[int]error{},
		boot:    bootA,
	}
	s := &stops{results: map[int][]error{14: {os.ErrProcessDone}, 15: {errors.New("procutil: processes in the group outlived SIGKILL")}}}
	var lines logLines
	if err := sweepWith(reg, info, s.stop, probes(map[int]error{11: nil}), lines.logf); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"orphanreaper: sweep left group 11 (terminal): its leader 11 is gone but the group still has members",
		"orphanreaper: sweep left group 12 (headless browser): its leader 12 is gone and the group is empty",
		"orphanreaper: sweep left group 13 (codex read): pid 13 now names another process, or this sweep reads from another time namespace than the recorder: recorded {Boot:boot-a Start:6299260}, found {Boot:boot-a Start:1}",
		"orphanreaper: sweep left group 14 (mcp sign-in): it ended before the sweep signalled it",
		"orphanreaper: sweep kept group 15 (provider) for the next launch: could not end it: procutil: processes in the group outlived SIGKILL",
		"orphanreaper: swept 1 orphaned process group(s) from a previous run",
	}, "\n")
	if got := lines.text(); got != want {
		t.Fatalf("sweep logged\n%s\nwant\n%s", got, want)
	}
	if got := leaders(t, reg); len(got) != 1 || got[0] != 15 {
		t.Fatalf("after the sweep the registry holds %v, want only the group it could not end", got)
	}
	for _, pgid := range []int{11, 12, 13} {
		if s.of(pgid) != 0 {
			t.Errorf("group %d was left, yet stopped", pgid)
		}
	}
}

// A leader the sweep cannot read keeps its record and is not signalled; the
// next launch, which can read it, reaps it.
func TestASweepThatCannotReadALeaderRetriesItNextLaunch(t *testing.T) {
	reg := seed(t, recorded("shell command", 10, 500))
	s := &stops{}
	var first logLines
	unreadable := fakeProc{readErr: map[int]error{10: errors.New("permission denied")}, boot: bootA}
	if err := sweepWith(reg, unreadable, s.stop, probes(nil), first.logf); err != nil {
		t.Fatal(err)
	}
	if s.of(10) != 0 || strings.Contains(first.text(), "swept") {
		t.Fatalf("an unreadable leader was stopped %d times; logged %q", s.of(10), first.text())
	}
	if got := leaders(t, reg); len(got) != 1 || got[0] != 10 {
		t.Fatalf("after the first sweep the registry holds %v, want the unreadable group", got)
	}
	var second logLines
	readable := fakeProc{procs: map[int]instanceinfo.Process{10: running(1)}, boot: bootA}
	if err := sweepWith(reg, readable, s.stop, probes(nil), second.logf); err != nil {
		t.Fatal(err)
	}
	if s.of(10) != 1 || second.text() != "orphanreaper: swept 1 orphaned process group(s) from a previous run" {
		t.Fatalf("the second sweep stopped the group %d times and logged %q", s.of(10), second.text())
	}
	if got := leaders(t, reg); len(got) != 0 {
		t.Fatalf("after the group ended the registry holds %v", got)
	}
}

// A group whose signals all failed is kept and not counted; the next launch
// ends it.
func TestAGroupTheSweepCouldNotEndIsRetriedNextLaunch(t *testing.T) {
	reg := seed(t, recorded("shell command", 10, 500))
	info := fakeProc{procs: map[int]instanceinfo.Process{10: running(1)}, boot: bootA}
	s := &stops{results: map[int][]error{10: {errors.New("operation not permitted")}}}
	var first logLines
	if err := sweepWith(reg, info, s.stop, probes(nil), first.logf); err != nil {
		t.Fatal(err)
	}
	if want := "orphanreaper: sweep kept group 10 (shell command) for the next launch: could not end it: operation not permitted"; first.text() != want {
		t.Fatalf("the first sweep logged %q, want %q", first.text(), want)
	}
	if got := leaders(t, reg); len(got) != 1 || got[0] != 10 {
		t.Fatalf("after the failed stop the registry holds %v, want the group", got)
	}
	var second logLines
	if err := sweepWith(reg, info, s.stop, probes(nil), second.logf); err != nil {
		t.Fatal(err)
	}
	if s.of(10) != 2 || second.text() != "orphanreaper: swept 1 orphaned process group(s) from a previous run" {
		t.Fatalf("the second sweep stopped the group %d times in all and logged %q", s.of(10), second.text())
	}
	if got := leaders(t, reg); len(got) != 0 {
		t.Fatalf("after the group ended the registry holds %v", got)
	}
}

// The sweep stops its groups at once, so a launch with several orphans
// waits one grace, not one per group: each stop here returns only once all
// of them are under way.
func TestSweepStopsItsGroupsAtOnce(t *testing.T) {
	const n = 4
	var recs []Record
	procs := map[int]instanceinfo.Process{}
	for pid := 10; pid < 10+n; pid++ {
		recs = append(recs, recorded("shell command", pid, 500))
		procs[pid] = running(1)
	}
	reg := seed(t, recs...)
	var started sync.WaitGroup
	started.Add(n)
	stop := func(int) error {
		started.Done()
		done := make(chan struct{})
		go func() { started.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other groups' stops never began")
		}
	}
	var lines logLines
	if err := sweepWith(reg, fakeProc{procs: procs, boot: bootA}, stop, probes(nil), lines.logf); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("orphanreaper: swept %d orphaned process group(s) from a previous run", n); lines.text() != want {
		t.Fatalf("sweep logged %q, want %q", lines.text(), want)
	}
}

func TestSweepNoRecordsNoError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	s := &stops{}
	var lines logLines
	if err := sweepWith(reg, fakeProc{}, s.stop, probes(nil), lines.logf); err != nil {
		t.Fatalf("sweepWith on empty registry: %v", err)
	}
	if len(s.stopped) != 0 || lines.text() != "" {
		t.Errorf("no records should mean no stops and no log, got %v %q", s.stopped, lines.text())
	}
}

// previousRecord and previousShouldReap are the record and decision rule of
// the version before identities, kept verbatim: an older build started on
// the same data directory reads this version's records with them.
type previousRecord struct {
	Owner      string `json:"owner"`
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	CreateUnix int64  `json:"create_unix"`
	ParentPID  int    `json:"parent_pid,omitempty"`
}

type previousProcInfo interface {
	Running(pid int) bool
	CreateUnix(pid int) (ms int64, ok bool)
	PPID(pid int) (ppid int, ok bool)
}

func previousShouldReap(rec previousRecord, info previousProcInfo) bool {
	if !info.Running(rec.PID) {
		return false
	}
	if rec.CreateUnix > 0 {
		if ct, ok := info.CreateUnix(rec.PID); !ok || ct != rec.CreateUnix {
			return false
		}
	}
	ppid, ok := info.PPID(rec.PID)
	if !ok {
		return false
	}
	if rec.ParentPID > 0 {
		return ppid != rec.ParentPID
	}
	return ppid == 1
}

// previousView reads a fakeProc as the earlier version's reader did.
type previousView struct{ fakeProc }

func (v previousView) Running(pid int) bool { _, ok := v.procs[pid]; return ok }
func (v previousView) CreateUnix(pid int) (int64, bool) {
	ms, err := v.LegacyStartMillis(pid)
	return ms, err == nil
}
func (v previousView) PPID(pid int) (int, bool) { p, ok := v.procs[pid]; return p.PPID, ok }

// An earlier version reading a record this version wrote still tells the
// recorded leader from a process that reused its pid.
func TestAnEarlierVersionNeverKillsAReusedPidFromANewRecord(t *testing.T) {
	recorder := fakeProc{procs: map[int]instanceinfo.Process{10: running(500)}, boot: bootA, legacy: map[int]int64{10: recordedMs}}
	c, err := capture(recorder, 10, 500)
	if err != nil {
		t.Fatal(err)
	}
	reg, path := newTestRegistry(t)
	if err := reg.Add("shell command", c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written []previousRecord
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].CreateUnix != recordedMs {
		t.Fatalf("the earlier version reads %+v from %s, want the recorded start time", written, data)
	}
	orphaned := fakeProc{procs: map[int]instanceinfo.Process{10: running(1)}, legacy: map[int]int64{10: recordedMs}}
	if !previousShouldReap(written[0], previousView{orphaned}) {
		t.Fatal("the earlier version left the recorded orphan running")
	}
	reused := fakeProc{procs: map[int]instanceinfo.Process{10: {Start: "9999999", PPID: 1}}, legacy: map[int]int64{10: recordedMs + 47_000}}
	if previousShouldReap(written[0], previousView{reused}) {
		t.Fatal("the earlier version would kill a process that reused the recorded pid")
	}
}
