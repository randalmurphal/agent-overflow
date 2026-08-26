package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

// deadPID returns a pid that is not running. High pids are handed out
// last, so scanning down from the Linux default pid_max finds a free one
// immediately; the loop exists so a machine that has wrapped around does
// not make the test flaky.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 4194303; pid > 4190000; pid-- {
		if !instanceinfo.ProcessAlive(pid) {
			return pid
		}
	}
	t.Skip("no free pid to use as a dead process")
	return 0
}

// seedInstance writes both discovery files for one instance: the
// registry row a listing finds, and the data-dir instance file that
// carries the token. Either may be omitted to build the half-states
// resolution and pruning have to survive.
func seedInstance(t *testing.T, registryDir, dataRoot string, pid int, opts ...func(*instanceinfo.Row)) string {
	t.Helper()
	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := instanceinfo.ID(dataRoot)
	row := instanceinfo.Row{
		Identity: instanceinfo.Identity{ID: id, Mode: instanceinfo.ModeHarness, StartedAt: "2026-08-26T00:00:00Z"},
		PID:      pid,
		Port:     4321,
		DataRoot: dataRoot,
		DataDir:  dataDir,
	}
	for _, opt := range opts {
		opt(&row)
	}
	if err := instanceinfo.WriteIn(registryDir, row); err != nil {
		t.Fatal(err)
	}
	return id
}

func writeInstanceFile(t *testing.T, dataRoot string, pid int) {
	t.Helper()
	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bs := harnessclient.Bootstrap{URL: "http://127.0.0.1:4321", Port: 4321, Token: "t", PID: pid, DataRoot: dataRoot, DataDir: dataDir}
	data, err := json.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harnessclient.InstanceFilePath(dataDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testEnv(registryDir string) (*env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &env{stdout: &stdout, stderr: &stderr, format: "text", registryDir: registryDir}, &stdout, &stderr
}

func TestResolveTargetPrefersTheNamedInstanceID(t *testing.T) {
	registry := t.TempDir()
	wanted := seedInstance(t, registry, t.TempDir(), os.Getpid())
	seedInstance(t, registry, t.TempDir(), os.Getpid())

	e, _, _ := testEnv(registry)
	e.instance = wanted
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != wanted {
		t.Fatalf("resolved %s, want %s", got.ID, wanted)
	}
	if got.Row == nil {
		t.Fatal("a resolved registry row should carry its row")
	}
}

// A data root resolves even with no registry row, because that is the
// state `up` runs in.
func TestResolveTargetAcceptsADataRootWithNoRow(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()

	e, _, _ := testEnv(registry)
	e.instance = root
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != instanceinfo.ID(root) {
		t.Fatalf("id = %s, want %s", got.ID, instanceinfo.ID(root))
	}
	if got.DataDir != filepath.Join(root, "agent-overflow") {
		t.Fatalf("dataDir = %s", got.DataDir)
	}
	if got.Row != nil {
		t.Fatal("no row exists for this root; Row should be nil")
	}
}

func TestResolveTargetPicksTheOnlyLiveRow(t *testing.T) {
	registry := t.TempDir()
	live := seedInstance(t, registry, t.TempDir(), os.Getpid())
	seedInstance(t, registry, t.TempDir(), deadPID(t))

	e, _, _ := testEnv(registry)
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != live {
		t.Fatalf("resolved %s, want the live row %s", got.ID, live)
	}
}

// Guessing between two live instances would send a reset at the wrong
// one, so ambiguity is an error that lists what it saw.
func TestResolveTargetRefusesTwoLiveInstances(t *testing.T) {
	registry := t.TempDir()
	first := seedInstance(t, registry, t.TempDir(), os.Getpid())
	second := seedInstance(t, registry, t.TempDir(), os.Getpid())

	e, _, _ := testEnv(registry)
	_, err := e.resolveTarget()
	if err == nil {
		t.Fatal("want an ambiguity error")
	}
	for _, id := range []string{first, second} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error does not list candidate %s: %v", id, err)
		}
	}
	if !strings.Contains(err.Error(), "--instance") {
		t.Errorf("error does not say how to disambiguate: %v", err)
	}
}

func TestResolveTargetFallsBackToThisWorktreesDefault(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != instanceinfo.DefaultDataRoot() {
		t.Fatalf("dataRoot = %s, want the worktree default %s", got.DataRoot, instanceinfo.DefaultDataRoot())
	}
}

// A dead row still resolves by id (down/list want it); only a selector
// that looks like an id and matches nothing is an error.
func TestResolveTargetErrorNamesAnUnknownInstanceID(t *testing.T) {
	registry := t.TempDir()
	existing := seedInstance(t, registry, t.TempDir(), os.Getpid())

	e, _, _ := testEnv(registry)
	e.instance = "deadbeef"
	_, err := e.resolveTarget()
	if err == nil {
		t.Fatal("want an error for an unknown id")
	}
	if !strings.Contains(err.Error(), "deadbeef") || !strings.Contains(err.Error(), existing) {
		t.Fatalf("error should name both the miss and the candidates: %v", err)
	}
}

func TestListPrunesAStaleRowWithNoInstanceFile(t *testing.T) {
	registry := t.TempDir()
	id := seedInstance(t, registry, t.TempDir(), deadPID(t))

	e, stdout, _ := testEnv(registry)
	if err := runList(e, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "pruned stale row "+id) {
		t.Fatalf("row was not pruned:\n%s", stdout.String())
	}
	rows, err := instanceinfo.ListIn(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("registry still holds %d rows", len(rows))
	}
}

func TestListPrunesAStaleRowWhoseInstanceFileNamesTheSameDeadPID(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	dead := deadPID(t)
	id := seedInstance(t, registry, root, dead)
	writeInstanceFile(t, root, dead)

	e, stdout, _ := testEnv(registry)
	if err := runList(e, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "pruned stale row "+id) {
		t.Fatalf("row was not pruned:\n%s", stdout.String())
	}
}

// The row is discovery state about a DATA ROOT. If that root's own file
// names a different, living process, the row is not ours to delete.
func TestListKeepsAStaleRowWhoseDataRootNamesALiveProcess(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, deadPID(t))
	writeInstanceFile(t, root, os.Getpid())

	e, stdout, _ := testEnv(registry)
	if err := runList(e, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "pruned") {
		t.Fatalf("row should have been kept:\n%s", stdout.String())
	}
	rows, err := instanceinfo.ListIn(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("registry rows = %+v", rows)
	}
}

// A pid is a number the OS recycles; a registry row is written once and
// never revised. `down` sends SIGTERM and then SIGKILL, so the row's word
// alone is not enough — the data root's own instance file has to name the
// same pid, or the CLI is one stale row away from killing whatever
// inherited the number.
func TestDownRefusesAPIDTheDataRootDoesNotClaim(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	// The row claims THIS process (alive, so the row is not stale), while
	// the data root names somebody else. Signalling here would kill the
	// test runner.
	seedInstance(t, registry, root, os.Getpid())
	writeInstanceFile(t, root, os.Getpid()+1)

	e, _, _ := testEnv(registry)
	err := runDown(e, nil)
	if err == nil {
		t.Fatal("down signalled a pid the data root does not claim")
	}
	if !strings.Contains(err.Error(), "refusing to signal") {
		t.Fatalf("error = %v", err)
	}
	// The message has to name the root, because that is where the reader
	// looks to work out which of the two claims is stale.
	if !strings.Contains(err.Error(), filepath.Join(root, "agent-overflow")) {
		t.Errorf("error does not name the data dir it read: %v", err)
	}
}

// The same rule under --all: one unconfirmable row is reported, and the
// confirmable ones are still stopped rather than the whole sweep aborting.
func TestDownAllReportsUnconfirmableRowsWithoutSignallingThem(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())
	// No instance file at all: nothing claims this root.

	e, stdout, _ := testEnv(registry)
	err := runDown(e, []string{"--all"})
	if err == nil {
		t.Fatal("down --all signalled a pid nothing confirms")
	}
	if !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "refusing to signal") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(stdout.String(), "stopped") {
		t.Fatalf("nothing should have been stopped:\n%s", stdout.String())
	}
}

// The confirmable case still resolves a pid, which is what proves the
// guard did not simply refuse everything.
func TestDownAcceptsAPIDTheDataRootConfirms(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	seedInstance(t, registry, root, os.Getpid())
	writeInstanceFile(t, root, os.Getpid())

	e, _, _ := testEnv(registry)
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := pidFor(got)
	if err != nil {
		t.Fatalf("a row confirmed by its own data root must resolve: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", pid, os.Getpid())
	}
}

func TestUpRefusesASecondInstanceOnALiveDataRoot(t *testing.T) {
	root := t.TempDir()
	writeInstanceFile(t, root, os.Getpid())

	e, _, _ := testEnv(t.TempDir())
	err := refuseSecondInstance(e, root)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error = %v", err)
	}
}

// A killed instance leaves its file behind. Refusing there would make a
// crash require manual cleanup.
func TestUpAllowsABootOverADeadInstanceFile(t *testing.T) {
	root := t.TempDir()
	writeInstanceFile(t, root, deadPID(t))

	e, _, _ := testEnv(t.TempDir())
	if err := refuseSecondInstance(e, root); err != nil {
		t.Fatalf("boot over a dead instance file should be allowed: %v", err)
	}
}
