package orphanreaper

import (
	"syscall"
	"testing"
)

// fakeProc is a deterministic ProcInfo keyed by pid.
type fakeProc struct {
	running    map[int]bool
	createUnix map[int]int64
	ppid       map[int]int
}

func (f fakeProc) Running(pid int) bool { return f.running[pid] }
func (f fakeProc) CreateUnix(pid int) (int64, bool) {
	v, ok := f.createUnix[pid]
	return v, ok
}
func (f fakeProc) PPID(pid int) (int, bool) {
	v, ok := f.ppid[pid]
	return v, ok
}

func TestShouldReap(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		info fakeProc
		want bool
	}{
		{
			name: "alive, start-time match, orphaned -> reap",
			rec:  Record{PID: 10, PGID: 10, CreateUnix: 100},
			info: fakeProc{running: map[int]bool{10: true}, createUnix: map[int]int64{10: 100}, ppid: map[int]int{10: 1}},
			want: true,
		},
		{
			name: "dead -> skip",
			rec:  Record{PID: 11, PGID: 11, CreateUnix: 100},
			info: fakeProc{running: map[int]bool{11: false}},
			want: false,
		},
		{
			name: "start-time mismatch (PID reused) -> skip",
			rec:  Record{PID: 12, PGID: 12, CreateUnix: 100},
			info: fakeProc{running: map[int]bool{12: true}, createUnix: map[int]int64{12: 999}, ppid: map[int]int{12: 1}},
			want: false,
		},
		{
			name: "still parented (not orphaned) -> skip",
			rec:  Record{PID: 13, PGID: 13, CreateUnix: 100},
			info: fakeProc{running: map[int]bool{13: true}, createUnix: map[int]int64{13: 100}, ppid: map[int]int{13: 4242}},
			want: false,
		},
		{
			name: "no recorded start-time falls back to orphan check -> reap",
			rec:  Record{PID: 14, PGID: 14, CreateUnix: 0},
			info: fakeProc{running: map[int]bool{14: true}, ppid: map[int]int{14: 1}},
			want: true,
		},
		{
			name: "no recorded start-time but still parented -> skip",
			rec:  Record{PID: 15, PGID: 15, CreateUnix: 0},
			info: fakeProc{running: map[int]bool{15: true}, ppid: map[int]int{15: 500}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldReap(tc.rec, tc.info); got != tc.want {
				t.Errorf("shouldReap = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSweepKillsOrphansAndClears(t *testing.T) {
	reg, _ := newTestRegistry(t)
	// 10 is a reapable orphan; 11 is still parented (alive but not ours to kill).
	_ = reg.Add(Record{PID: 10, PGID: 10, CreateUnix: 100})
	_ = reg.Add(Record{PID: 11, PGID: 11, CreateUnix: 100})
	info := fakeProc{
		running:    map[int]bool{10: true, 11: true},
		createUnix: map[int]int64{10: 100, 11: 100},
		ppid:       map[int]int{10: 1, 11: 9000},
	}
	k := &recordingKiller{}

	if err := sweepWith(reg, info, k.kill, 0); err != nil {
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

func TestSweepNoRecordsNoError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	k := &recordingKiller{}
	if err := sweepWith(reg, fakeProc{}, k.kill, 0); err != nil {
		t.Fatalf("sweepWith on empty registry: %v", err)
	}
	if len(k.calls) != 0 {
		t.Errorf("no records should mean no kills, got %v", k.calls)
	}
}
