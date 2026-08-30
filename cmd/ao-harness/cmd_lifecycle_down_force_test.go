package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// forcedStopBudget bounds every wait this file performs. A --force stop
// that has not resolved inside it is a failure, never a longer wait.
const forcedStopBudget = 15 * time.Second

// fakeProbe answers the three /proc questions from canned values, so the
// decision table below covers pid states no test may create for real
// (a live foreign process, a recycled pid) without inspecting, let alone
// signalling, anything it does not own.
func fakeProbe(alive bool, identity instanceinfo.ProcessIdentity, identityErr error, comm string, commErr error) forceProbe {
	return forceProbe{
		alive:    func(int) bool { return alive },
		identity: func(int) (instanceinfo.ProcessIdentity, error) { return identity, identityErr },
		comm:     func(int) (string, error) { return comm, commErr },
	}
}

func ourNamespace() string { return instanceinfo.CurrentPIDNamespace() }

// --force is a decision, not a permission. This table is that decision:
// the row's evidence and the pid's actual /proc evidence in, one of
// refuse / prune / stop out.
func TestDecideForcedStop(t *testing.T) {
	ns := ourNamespace()
	ours := instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/bin/agent-overflow", Namespace: ns}

	base := func(mutate func(*instanceinfo.Row)) instanceinfo.Row {
		row := instanceinfo.Row{
			Identity: instanceinfo.Identity{ID: "abcdef01", Mode: instanceinfo.ModeHarness, PIDNamespace: ns},
			PID:      4242,
			DataRoot: "/tmp/root",
		}
		if mutate != nil {
			mutate(&row)
		}
		return row
	}

	cases := []struct {
		name    string
		row     instanceinfo.Row
		probe   forceProbe
		want    forceVerdict
		mustSay string
	}{
		{
			name:  "a confirmed agent-overflow process is stopped",
			row:   base(nil),
			probe: fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:  forceStop,
		},
		{
			name: "the row's recorded identity is checked when it has one",
			row: base(func(r *instanceinfo.Row) {
				r.ProcessStartTime, r.ExecutablePath = ours.StartTime, ours.Executable
			}),
			probe: fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:  forceStop,
		},
		{
			name:  "a kernel-truncated name still matches by prefix",
			row:   base(nil),
			probe: fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/agent-overflow-harness", Namespace: ns}, nil, "agent-overflow-", nil),
			want:  forceStop,
		},
		{
			name:  "a binary replaced on disk is still ours",
			row:   base(nil),
			probe: fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/agent-overflow (deleted)", Namespace: ns}, nil, "agent-overflow", nil),
			want:  forceStop,
		},
		{
			name:  "a dead pid is pruned, not signalled",
			row:   base(nil),
			probe: fakeProbe(false, instanceinfo.ProcessIdentity{}, errors.New("no such process"), "", errors.New("no such process")),
			want:  forcePruneOnly,
		},
		{
			name:    "an unrelated process is refused and named",
			row:     base(nil),
			probe:   fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/usr/bin/sleep", Namespace: ns}, nil, "sleep", nil),
			want:    forceRefuse,
			mustSay: "sleep",
		},
		{
			name:    "a name that matches over a foreign executable is refused",
			row:     base(nil),
			probe:   fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/bin/sh", Namespace: ns}, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "/bin/sh",
		},
		{
			name: "a recycled pid is refused however much it looks like us",
			row: base(func(r *instanceinfo.Row) {
				r.ProcessStartTime, r.ExecutablePath = "111", ours.Executable
			}),
			probe:   fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "recycled",
		},
		{
			name: "a different executable than the row recorded is refused",
			row: base(func(r *instanceinfo.Row) {
				r.ProcessStartTime, r.ExecutablePath = ours.StartTime, "/elsewhere/agent-overflow"
			}),
			probe:   fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "/elsewhere/agent-overflow",
		},
		{
			name:    "an unreadable /proc entry is refused, never assumed",
			row:     base(nil),
			probe:   fakeProbe(true, instanceinfo.ProcessIdentity{}, errors.New("permission denied"), "", nil),
			want:    forceRefuse,
			mustSay: "permission denied",
		},
		{
			name:    "an unreadable name is refused, never assumed",
			row:     base(nil),
			probe:   fakeProbe(true, ours, nil, "", errors.New("stat vanished")),
			want:    forceRefuse,
			mustSay: "stat vanished",
		},
		{
			name:    "a row from another pid namespace is refused",
			row:     base(func(r *instanceinfo.Row) { r.PIDNamespace = "pid:[4026999999]" }),
			probe:   fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "namespace",
		},
		{
			name:    "a process living in another namespace is refused",
			row:     base(nil),
			probe:   fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/bin/agent-overflow", Namespace: "pid:[4026999999]"}, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "namespace",
		},
		{
			name:    "a row with no pid is refused",
			row:     base(func(r *instanceinfo.Row) { r.PID = 0 }),
			probe:   fakeProbe(true, ours, nil, "agent-overflow", nil),
			want:    forceRefuse,
			mustSay: "not a pid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, identity, err := decideForcedStop(tc.row, tc.probe)
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v (err %v)", got, tc.want, err)
			}
			switch tc.want {
			case forceStop:
				if err != nil {
					t.Fatalf("stop verdict carried an error: %v", err)
				}
				if identity.StartTime == "" || identity.Executable == "" || identity.Namespace == "" {
					// The escalation re-verifies this identity, so an
					// incomplete one would degrade to a PID-only kill.
					t.Fatalf("stop verdict returned incomplete identity %+v", identity)
				}
			case forceRefuse:
				if err == nil {
					t.Fatal("refusal carried no reason")
				}
				if tc.mustSay != "" && !strings.Contains(err.Error(), tc.mustSay) {
					t.Fatalf("refusal does not say %q: %v", tc.mustSay, err)
				}
			case forcePruneOnly:
				if err != nil {
					t.Fatalf("prune verdict carried an error: %v", err)
				}
			}
		})
	}
}

// Without --force nothing changes: the refusal stands and the row stays,
// because a row `down` refused to act on is still the only record that
// the pid was ever ours.
func TestDownWithoutForceRefusesAndKeepsTheRow(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())

	e, _, _ := testEnv(registry)
	e.instance = id
	err := runDown(e, nil)
	if err == nil {
		t.Fatal("down signalled a pid nothing confirms")
	}
	if !strings.Contains(err.Error(), "refusing to signal") {
		t.Fatalf("error = %v", err)
	}
	// The refusal has to name the way out, or the operator is stranded
	// with a pid they must find and kill by hand.
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal does not mention the escape hatch: %v", err)
	}
	if rows, listErr := instanceinfo.ListIn(registry, nil); listErr != nil || len(rows) != 1 {
		t.Fatalf("row should have survived: rows=%v err=%v", rows, listErr)
	}
}

// --force is not a permission to signal whatever the row says. The pid
// here is this very test process, which is alive and named in the row;
// only the /proc evidence stops it being killed.
func TestDownForceRefusesAPIDThatIsNotOurs(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())

	restoreProbe(t, fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/usr/bin/sleep", Namespace: ourNamespace()}, nil, "sleep", nil))
	signalled := stubTerminate(t)

	e, _, _ := testEnv(registry)
	e.instance = id
	err := runDown(e, []string{"--force"})
	if err == nil {
		t.Fatal("down --force signalled a pid /proc says is not ours")
	}
	if !strings.Contains(err.Error(), "sleep") {
		t.Fatalf("refusal does not name what the pid actually is: %v", err)
	}
	if *signalled != 0 {
		t.Fatalf("a refused pid was signalled %d times", *signalled)
	}
	if rows, listErr := instanceinfo.ListIn(registry, nil); listErr != nil || len(rows) != 1 {
		t.Fatalf("a refused row must survive: rows=%v err=%v", rows, listErr)
	}
}

func TestDownForceStopsAConfirmedPIDAndPrunesTheRow(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())

	restoreProbe(t, fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/bin/agent-overflow", Namespace: ourNamespace()}, nil, "agent-overflow", nil))
	signalled := stubTerminate(t)

	e, stdout, _ := testEnv(registry)
	e.instance = id
	if err := runDown(e, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if *signalled != 1 {
		t.Fatalf("confirmed pid was signalled %d times, want 1", *signalled)
	}
	if !strings.Contains(stdout.String(), "stopped") || !strings.Contains(stdout.String(), "pruned") {
		t.Fatalf("output does not report the stop and the prune:\n%s", stdout.String())
	}
	rows, listErr := instanceinfo.ListIn(registry, nil)
	if listErr != nil || len(rows) != 0 {
		t.Fatalf("row was not pruned: rows=%v err=%v", rows, listErr)
	}
}

// A dead pid needs no signal at all, and the row is then pure leftovers.
func TestDownForcePrunesARowWhosePIDIsGone(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, deadPID(t))

	signalled := stubTerminate(t)

	e, stdout, _ := testEnv(registry)
	e.instance = id
	if err := runDown(e, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if *signalled != 0 {
		t.Fatalf("a dead pid was signalled %d times", *signalled)
	}
	if !strings.Contains(stdout.String(), "already dead") || !strings.Contains(stdout.String(), "pruned") {
		t.Fatalf("output does not say the pid was already gone:\n%s", stdout.String())
	}
	if rows, listErr := instanceinfo.ListIn(registry, nil); listErr != nil || len(rows) != 0 {
		t.Fatalf("row was not pruned: rows=%v err=%v", rows, listErr)
	}
}

// --force overrides ONE refusal: nothing claims the root. A root that
// names a DIFFERENT pid is a contradiction, not missing evidence, and
// force must not touch it.
func TestDownForceStillRefusesAContradictedRow(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())
	writeInstanceFile(t, root, os.Getpid()+1)

	restoreProbe(t, fakeProbe(true, instanceinfo.ProcessIdentity{StartTime: "900", Executable: "/opt/ao/bin/agent-overflow", Namespace: ourNamespace()}, nil, "agent-overflow", nil))
	signalled := stubTerminate(t)

	e, _, _ := testEnv(registry)
	e.instance = id
	err := runDown(e, []string{"--force"})
	if err == nil || !strings.Contains(err.Error(), "does not claim") {
		t.Fatalf("error = %v", err)
	}
	if *signalled != 0 {
		t.Fatalf("a contradicted row was signalled %d times", *signalled)
	}
}

// The one case that touches a real process: a child this test starts and
// owns, exec'd through a copy of a harmless binary under our name, so
// every /proc read is the real one.
func TestForcedStopTerminatesARealProcessItOwns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the forced-stop evidence is /proc, which only Linux has")
	}
	pid, done := startSleeperNamed(t, harnessProcessName)
	row := instanceinfo.Row{
		Identity: instanceinfo.Identity{ID: "abcdef01", Mode: instanceinfo.ModeHarness, PIDNamespace: ourNamespace()},
		PID:      pid,
		DataRoot: t.TempDir(),
	}

	verdict, identity, err := decideForcedStop(row, forcedProbe)
	if err != nil || verdict != forceStop {
		t.Fatalf("verdict = %v, identity = %+v, err = %v", verdict, identity, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), forcedStopBudget)
	defer cancel()
	signalled, err := stopForcedVictim(ctx, row, forcedProbe)
	if err != nil || !signalled {
		t.Fatalf("stopForcedVictim = %v, %v", signalled, err)
	}
	select {
	case <-done:
	case <-time.After(forcedStopBudget):
		t.Fatal("the child outlived the forced stop budget")
	}
}

// The mirror of the case above on the same real /proc: an unrelated
// child, under its own name, is refused and named.
func TestForcedStopRefusesARealProcessThatIsNotOurs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the forced-stop evidence is /proc, which only Linux has")
	}
	pid, _ := startSleeperNamed(t, "not-agent-overflow")
	row := instanceinfo.Row{
		Identity: instanceinfo.Identity{ID: "abcdef01", Mode: instanceinfo.ModeHarness, PIDNamespace: ourNamespace()},
		PID:      pid,
		DataRoot: t.TempDir(),
	}
	verdict, _, err := decideForcedStop(row, forcedProbe)
	if verdict != forceRefuse {
		t.Fatalf("verdict = %v, want refuse", verdict)
	}
	if err == nil || !strings.Contains(err.Error(), "not-agent-overflow") {
		t.Fatalf("refusal does not name the process: %v", err)
	}
}

// startSleeperNamed runs a long sleep from a COPY of the system sleep
// binary placed under `name`, which is what makes both /proc facts the
// force path reads real: comm comes from the path handed to execve, and
// /proc/<pid>/exe resolves to the copy.
func startSleeperNamed(t *testing.T, name string) (int, <-chan struct{}) {
	t.Helper()
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary to copy")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Skipf("cannot read %s: %v", source, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "600")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec %s: %v", path, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(forcedStopBudget):
			t.Errorf("child %d survived teardown", cmd.Process.Pid)
		}
	})
	return cmd.Process.Pid, done
}

func restoreProbe(t *testing.T, probe forceProbe) {
	t.Helper()
	previous := forcedProbe
	forcedProbe = probe
	t.Cleanup(func() { forcedProbe = previous })
}

// stubTerminate replaces the escalation with a counter: no test in this
// package may signal a process it did not start, and a table-driven one
// cannot start the process it is describing.
func stubTerminate(t *testing.T) *int {
	t.Helper()
	calls := 0
	previous := terminateForced
	terminateForced = func(_ context.Context, pid int, expected instanceinfo.ProcessIdentity, _ time.Duration) error {
		calls++
		if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
			return fmt.Errorf("terminate %d: incomplete identity %+v", pid, expected)
		}
		return nil
	}
	t.Cleanup(func() { terminateForced = previous })
	return &calls
}
