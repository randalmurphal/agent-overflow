package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

// fakeInterop points the two Windows tool paths at real files in a temp
// dir (so interopAvailable is satisfied off Windows) and records every
// invocation, answering tasklist with the supplied CSV.
type fakeInterop struct {
	calls    [][]string
	tasklist string
	taskErr  error
	killErr  error
}

func (f *fakeInterop) install(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	tasklist := filepath.Join(dir, "tasklist.exe")
	taskkill := filepath.Join(dir, "taskkill.exe")
	for _, path := range []string{tasklist, taskkill} {
		if err := os.WriteFile(path, []byte("stub"), 0o700); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	origList, origKill, origRun := winTasklistExe, winTaskkillExe, runInterop
	t.Cleanup(func() { winTasklistExe, winTaskkillExe, runInterop = origList, origKill, origRun })
	winTasklistExe, winTaskkillExe = tasklist, taskkill
	runInterop = func(_ context.Context, exe string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{exe}, args...))
		if exe == winTaskkillExe {
			return nil, f.killErr
		}
		return []byte(f.tasklist), f.taskErr
	}
}

func (f *fakeInterop) killed() bool {
	for _, call := range f.calls {
		if call[0] == winTaskkillExe {
			return true
		}
	}
	return false
}

// The happy path: the pid answers with an agent-overflow image name, so
// the window is killed. The dev launcher is timestamp-named, which is
// why the check is a prefix.
func TestStopLauncherWindowKillsAConfirmedLauncher(t *testing.T) {
	fake := &fakeInterop{tasklist: `"agent-overflow-dev-20260826101112-4242.exe","4242","Console","1","512,345 K"`}
	fake.install(t)

	killed, note := stopLauncherWindow(4242)
	if !killed {
		t.Fatalf("launcher was not killed (note %q)", note)
	}
	if note != "" {
		t.Errorf("note = %q, want empty on a clean kill", note)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %v, want a tasklist then a taskkill", fake.calls)
	}
	if got := strings.Join(fake.calls[0], " "); !strings.Contains(got, "PID eq 4242") || !strings.Contains(got, "CSV") {
		t.Errorf("tasklist call = %q, want a CSV query filtered to the pid", got)
	}
	if got := strings.Join(fake.calls[1][1:], " "); got != "/PID 4242 /F" {
		t.Errorf("taskkill args = %q, want /PID 4242 /F", got)
	}
}

// A pid is a number Windows recycles and the value comes out of a file
// another process wrote, so an image name that is not ours must never be
// force-killed — the operator gets a sentence instead.
func TestStopLauncherWindowRefusesAForeignImage(t *testing.T) {
	fake := &fakeInterop{tasklist: `"explorer.exe","4242","Console","1","98,765 K"`}
	fake.install(t)

	killed, note := stopLauncherWindow(4242)
	if killed || fake.killed() {
		t.Fatal("a foreign image was force-killed")
	}
	if !strings.Contains(note, "explorer.exe") || !strings.Contains(note, "refusing") {
		t.Errorf("note = %q, want a refusal naming the image it found", note)
	}
}

// No WSL interop (a Linux or macOS host, or a stripped Windows install):
// nothing is run at all, and the note says the window is still up.
func TestStopLauncherWindowWithoutInteropTellsTheOperator(t *testing.T) {
	fake := &fakeInterop{}
	fake.install(t)
	winTasklistExe = filepath.Join(t.TempDir(), "absent-tasklist.exe")
	winTaskkillExe = filepath.Join(t.TempDir(), "absent-taskkill.exe")

	killed, note := stopLauncherWindow(4242)
	if killed {
		t.Fatal("killed without interop")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("ran %v with no interop available", fake.calls)
	}
	if !strings.Contains(note, "close it yourself") {
		t.Errorf("note = %q, want the operator told how the window closes", note)
	}
}

// The launcher exiting on its own is the ordinary case, not a problem to
// report: tasklist's "no tasks match" notice is not CSV, and must not be
// mistaken for an image name.
func TestStopLauncherWindowIsQuietWhenThePIDIsGone(t *testing.T) {
	fake := &fakeInterop{tasklist: "INFO: No tasks are running which match the specified criteria."}
	fake.install(t)

	killed, note := stopLauncherWindow(4242)
	if killed || fake.killed() {
		t.Fatal("killed a pid tasklist does not know")
	}
	if note != "" {
		t.Errorf("note = %q, want silence for a launcher that already exited", note)
	}
}

// A tasklist that fails is not licence to kill; the note carries the
// manual command.
func TestStopLauncherWindowRefusesWhenIdentificationFails(t *testing.T) {
	fake := &fakeInterop{taskErr: errors.New("interop refused")}
	fake.install(t)

	killed, note := stopLauncherWindow(4242)
	if killed || fake.killed() {
		t.Fatal("killed a pid that could not be identified")
	}
	if !strings.Contains(note, "taskkill.exe /PID 4242 /F") {
		t.Errorf("note = %q, want the manual command", note)
	}
}

// Which of the two discovery files answers "who hosts the window". The
// instance file is written by the running backend and withdrawn on
// shutdown, so it wins; the row is the fallback for a root whose file is
// unreadable.
func TestLauncherPIDPrefersTheInstanceFile(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent-overflow")
	writeInstanceFile(t, root, os.Getpid(), func(bs *harnessclient.Bootstrap) { bs.LauncherPid = 777 })
	row := &instanceinfo.Row{Identity: instanceinfo.Identity{LauncherPid: 999}}

	if got := launcherPIDFor(dataDir, row); got != 777 {
		t.Errorf("launcherPIDFor = %d, want the instance file's 777", got)
	}
	if got := launcherPIDFor(filepath.Join(t.TempDir(), "absent"), row); got != 999 {
		t.Errorf("launcherPIDFor with no instance file = %d, want the row's 999", got)
	}
	if got := launcherPIDFor(filepath.Join(t.TempDir(), "absent"), nil); got != 0 {
		t.Errorf("launcherPIDFor with nothing = %d, want 0", got)
	}
}

// A zero pid means no launcher hosts this instance (every shell but the
// Windows one). It must not probe interop at all.
func TestStopLauncherWindowIgnoresAnAbsentLauncher(t *testing.T) {
	fake := &fakeInterop{}
	fake.install(t)

	if killed, note := stopLauncherWindow(0); killed || note != "" {
		t.Fatalf("stopLauncherWindow(0) = %v, %q, want a silent no-op", killed, note)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("ran %v for an instance with no launcher", fake.calls)
	}
}

// Unparseable output is NOT "the process is gone". The two used to
// collapse into one answer, so a tasklist whose format changed (a locale,
// a newer Windows, a truncated pipe) reported a live launcher as already
// exited and left the window on the desktop with nothing said.
func TestUnparseableTasklistOutputIsAnErrorNotAnAbsence(t *testing.T) {
	for _, output := range []string{
		"Image Name                     PID Session Name", // the table form, not CSV
		"FEHLER: Der Prozess wurde nicht gefunden.",       // a localised notice we do not know
		`"agent-overflow.exe"`,                            // truncated: one field, not five
	} {
		fake := &fakeInterop{tasklist: output}
		fake.install(t)

		killed, note := stopLauncherWindow(4242)
		if killed || fake.killed() {
			t.Fatalf("killed on output %q", output)
		}
		if note == "" {
			t.Fatalf("output %q was read as `the process is gone` and reported nothing", output)
		}
		if !strings.Contains(note, "could not identify Windows pid 4242") {
			t.Errorf("note for %q = %q, want it to say identification failed", output, note)
		}
	}
}

// Empty output means the filter matched nothing, same as the notice.
func TestEmptyTasklistOutputIsAGoneProcess(t *testing.T) {
	fake := &fakeInterop{tasklist: "   \n"}
	fake.install(t)

	killed, note := stopLauncherWindow(4242)
	if killed || fake.killed() {
		t.Fatal("killed on empty tasklist output")
	}
	if note != "" {
		t.Errorf("note = %q, want silence", note)
	}
}

// A Windows tool that refuses says WHY on stderr, and Output() throws that
// away: every failure read "exit status 1", from a bad filter to a denied
// kill. runInterop splices the first stderr line back in.
func TestRunInteropCarriesTheToolsOwnStderr(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to act out a failing tool")
	}
	_, err := runInterop(context.Background(), "/bin/sh", "-c",
		"echo 'ERROR: Access is denied.' >&2; exit 1")
	if err == nil {
		t.Fatal("a failing tool reported success")
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("error = %v, want the tool's own stderr spliced in", err)
	}
}
