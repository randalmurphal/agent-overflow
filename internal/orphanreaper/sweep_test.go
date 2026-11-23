package orphanreaper

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// fakeProc is a deterministic ProcInfo keyed by pid. A pid absent from
// identity names no process.
type fakeProc struct {
	identity    map[int]Identity
	identityErr map[int]error
	// legacy is what gopsutil's wall-clock-derived start time reads now.
	legacy map[int]int64
	ppid   map[int]int
}

func (f fakeProc) Identify(pid int) (Identity, error) {
	if err := f.identityErr[pid]; err != nil {
		return Identity{}, err
	}
	id, ok := f.identity[pid]
	if !ok {
		return Identity{}, fmt.Errorf("process %d: %w", pid, ErrNoProcess)
	}
	return id, nil
}

func (f fakeProc) PPID(pid int) (int, error) {
	v, ok := f.ppid[pid]
	if !ok {
		return 0, fmt.Errorf("no parent for %d", pid)
	}
	return v, nil
}

func (f fakeProc) LegacyStartMillis(pid int) (int64, error) {
	v, ok := f.legacy[pid]
	if !ok {
		return 0, fmt.Errorf("no start time for %d", pid)
	}
	return v, nil
}

var leader = Identity{Boot: "boot-a", Start: "6299260"}

func recorded(id Identity) *Identity { return &id }

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		info fakeProc
		want verdict
		why  string
	}{
		{
			// The wall-clock start time reads a second off what it read when
			// the group was recorded, as a container's computed boot time
			// does; the kernel's values agree.
			name: "the recorded leader, whatever the wall clock says, orphaned -> reap",
			rec:  Record{PID: 10, PGID: 10, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{10: leader}, legacy: map[int]int64{10: 1_700_000_001_000}, ppid: map[int]int{10: 1}},
			want: verdict{reap: true},
		},
		{
			name: "same pid, another kernel start (pid reused) -> skip",
			rec:  Record{PID: 11, PGID: 11, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{11: {Boot: leader.Boot, Start: "7000000"}}, ppid: map[int]int{11: 1}},
			why:  "pid 11 is now another process",
		},
		{
			name: "same pid and start in another boot -> skip",
			rec:  Record{PID: 12, PGID: 12, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{12: {Boot: "boot-b", Start: leader.Start}}, ppid: map[int]int{12: 1}},
			why:  "pid 12 is now another process",
		},
		{
			name: "leader gone -> skip, probed",
			rec:  Record{PID: 13, PGID: 13, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{},
			want: verdict{leaderGone: true},
		},
		{
			name: "leader unreadable -> skip",
			rec:  Record{PID: 14, PGID: 14, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identityErr: map[int]error{14: errors.New("permission denied")}},
			why:  "reading its leader failed: permission denied",
		},
		{
			name: "still the recording parent's child -> skip",
			rec:  Record{PID: 15, PGID: 15, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{15: leader}, ppid: map[int]int{15: 500}},
			why:  "still the child of the recording process 500",
		},
		{
			name: "reparented to a subreaper -> reap",
			rec:  Record{PID: 16, PGID: 16, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{16: leader}, ppid: map[int]int{16: 77}},
			want: verdict{reap: true},
		},
		{
			name: "parent unreadable -> skip",
			rec:  Record{PID: 17, PGID: 17, Identity: recorded(leader), ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{17: leader}},
			why:  "reading its leader's parent failed",
		},
		{
			name: "no parent recorded, under init -> reap",
			rec:  Record{PID: 18, PGID: 18, Identity: recorded(leader)},
			info: fakeProc{identity: map[int]Identity{18: leader}, ppid: map[int]int{18: 1}},
			want: verdict{reap: true},
		},
		{
			name: "no parent recorded, not under init -> skip",
			rec:  Record{PID: 19, PGID: 19, Identity: recorded(leader)},
			info: fakeProc{identity: map[int]Identity{19: leader}, ppid: map[int]int{19: 4242}},
			why:  "parent is 4242, not init",
		},
		{
			name: "an earlier version's record whose start time matches -> reap",
			rec:  Record{PID: 20, PGID: 20, CreateUnix: 1_700_000_000_000, ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{20: leader}, legacy: map[int]int64{20: 1_700_000_000_000}, ppid: map[int]int{20: 1}},
			want: verdict{reap: true},
		},
		{
			name: "an earlier version's record whose start time differs -> skip",
			rec:  Record{PID: 21, PGID: 21, CreateUnix: 1_700_000_000_000, ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{21: leader}, legacy: map[int]int64{21: 1_700_000_009_000}, ppid: map[int]int{21: 1}},
			why:  "recorded start 1700000000000 ms, found 1700000009000 ms",
		},
		{
			name: "an earlier version's record whose leader is gone -> skip, probed",
			rec:  Record{PID: 22, PGID: 22, CreateUnix: 1_700_000_000_000, ParentPID: 500},
			info: fakeProc{},
			want: verdict{leaderGone: true},
		},
		{
			name: "a record naming no identity -> skip, even orphaned",
			rec:  Record{PID: 23, PGID: 23, ParentPID: 500},
			info: fakeProc{identity: map[int]Identity{23: leader}, legacy: map[int]int64{23: 1}, ppid: map[int]int{23: 1}},
			why:  "the record names no process identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judge(tc.rec, tc.info)
			if got.reap != tc.want.reap || got.leaderGone != tc.want.leaderGone {
				t.Fatalf("judge = %+v, want %+v", got, tc.want)
			}
			if tc.why != "" && !strings.Contains(got.why, tc.why) {
				t.Fatalf("judge left the group because %q, want %q", got.why, tc.why)
			}
			if !got.reap && !got.leaderGone && got.why == "" {
				t.Fatalf("judge left the group without a reason: %+v", got)
			}
		})
	}
}

func TestSweepKillsOrphansAndClears(t *testing.T) {
	reg, _ := newTestRegistry(t)
	// 10 is a reapable orphan; 11 is still parented (alive but not ours to kill).
	_ = reg.Add(Record{PID: 10, PGID: 10, Identity: recorded(leader), ParentPID: 500})
	_ = reg.Add(Record{PID: 11, PGID: 11, Identity: recorded(leader), ParentPID: 500})
	info := fakeProc{
		identity: map[int]Identity{10: leader, 11: leader},
		ppid:     map[int]int{10: 1, 11: 500},
	}
	k := &recordingKiller{}

	if err := sweepWith(reg, info, k.kill, (&logLines{}).logf, 0); err != nil {
		t.Fatalf("sweepWith: %v", err)
	}

	if got := k.signalsFor(10); len(got) != 2 || got[0] != syscall.SIGTERM || got[1] != syscall.SIGKILL {
		t.Errorf("pgid 10 signals = %v, want [SIGTERM SIGKILL]", got)
	}
	if got := k.signalsFor(11); len(got) != 0 {
		t.Errorf("pgid 11 is still parented; should not be killed, got %v", got)
	}

	// Registry is wiped regardless of what was killed.
	recs, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("registry after sweep = %+v, want cleared", recs)
	}
}

// Every group the sweep leaves is logged once, with its owner, pgid and
// why; a group whose leader is gone says whether it still has members.
func TestSweepLogsEachGroupItLeaves(t *testing.T) {
	reg, _ := newTestRegistry(t)
	for _, rec := range []Record{
		{Owner: "shell command", PID: 10, PGID: 10, Identity: recorded(leader), ParentPID: 500},
		{Owner: "terminal", PID: 11, PGID: 11, Identity: recorded(leader), ParentPID: 500},
		{Owner: "headless browser", PID: 12, PGID: 12, Identity: recorded(leader), ParentPID: 500},
		{Owner: "codex read", PID: 13, PGID: 13, Identity: recorded(leader), ParentPID: 500},
		{Owner: "mcp sign-in", PID: 14, PGID: 14, ParentPID: 500},
	} {
		if err := reg.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	info := fakeProc{
		identity: map[int]Identity{10: leader, 13: {Boot: leader.Boot, Start: "1"}, 14: leader},
		ppid:     map[int]int{10: 1, 13: 1, 14: 1},
	}
	k := &recordingKiller{probe: map[int]error{11: nil, 12: syscall.ESRCH}}
	var lines logLines
	if err := sweepWith(reg, info, k.kill, lines.logf, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"orphanreaper: sweep left group 11 (terminal): its leader 11 is gone but the group still has members",
		"orphanreaper: sweep left group 12 (headless browser): its leader 12 is gone and the group is empty",
		"orphanreaper: sweep left group 13 (codex read): pid 13 is now another process: recorded {Boot:boot-a Start:6299260}, found {Boot:boot-a Start:1}",
		"orphanreaper: sweep left group 14 (mcp sign-in): the record names no process identity",
		"orphanreaper: swept 1 orphaned process group(s) from a previous run",
	}
	if strings.Join(lines.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sweep logged\n%s\nwant\n%s", strings.Join(lines.lines, "\n"), strings.Join(want, "\n"))
	}
	for pgid := 11; pgid <= 14; pgid++ {
		if got := k.signalsFor(pgid); len(got) != 0 {
			t.Errorf("pgid %d was left, yet sent %v", pgid, got)
		}
	}
}

// A signal the sweep could not send is logged; one to a group that already
// ended is not a failure.
func TestSweepLogsASignalItCouldNotSend(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_ = reg.Add(Record{Owner: "shell command", PID: 10, PGID: 10, Identity: recorded(leader), ParentPID: 500})
	info := fakeProc{identity: map[int]Identity{10: leader}, ppid: map[int]int{10: 1}}
	k := &recordingKiller{fail: map[syscall.Signal]error{syscall.SIGTERM: syscall.EPERM, syscall.SIGKILL: syscall.ESRCH}}
	var lines logLines
	if err := sweepWith(reg, info, k.kill, lines.logf, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"orphanreaper: sweep could not send terminated to group 10 (shell command): operation not permitted",
		"orphanreaper: swept 1 orphaned process group(s) from a previous run",
	}
	if strings.Join(lines.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sweep logged\n%s\nwant\n%s", strings.Join(lines.lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestSweepNoRecordsNoError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	k := &recordingKiller{}
	if err := sweepWith(reg, fakeProc{}, k.kill, (&logLines{}).logf, 0); err != nil {
		t.Fatalf("sweepWith on empty registry: %v", err)
	}
	if len(k.calls) != 0 {
		t.Errorf("no records should mean no kills, got %v", k.calls)
	}
}

// logLines collects what the sweep logs.
type logLines struct {
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// recordingKiller captures (pgid, signal) pairs in call order, other than
// signal 0, which answers from probe (ESRCH when absent). fail answers a
// signal with an error.
type recordingKiller struct {
	mu    sync.Mutex
	probe map[int]error
	fail  map[syscall.Signal]error
	calls []struct {
		pgid int
		sig  syscall.Signal
	}
}

func (k *recordingKiller) kill(pgid int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if sig == 0 {
		if err, ok := k.probe[pgid]; ok {
			return err
		}
		return syscall.ESRCH
	}
	k.calls = append(k.calls, struct {
		pgid int
		sig  syscall.Signal
	}{pgid, sig})
	return k.fail[sig]
}

func (k *recordingKiller) signalsFor(pgid int) []syscall.Signal {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []syscall.Signal
	for _, c := range k.calls {
		if c.pgid == pgid {
			out = append(out, c.sig)
		}
	}
	return out
}
