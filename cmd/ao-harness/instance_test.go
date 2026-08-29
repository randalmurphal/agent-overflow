package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

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

func writeInstanceFile(t *testing.T, dataRoot string, pid int, opts ...func(*harnessclient.Bootstrap)) {
	t.Helper()
	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bs := harnessclient.Bootstrap{URL: "http://127.0.0.1:4321", Port: 4321, Token: "t", PID: pid, DataRoot: dataRoot, DataDir: dataDir}
	for _, opt := range opts {
		opt(&bs)
	}
	data, err := json.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harnessclient.InstanceFilePath(dataDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// exitCodeOf runs an error through the same mapping main() uses, so a
// test pins the CODE A SCRIPT SEES rather than the error type behind it.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return fail(&env{stdout: &stdout, stderr: &stderr}, err)
}

func testEnv(registryDir string) (*env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &env{stdout: &stdout, stderr: &stderr, format: "text", registryDir: registryDir}, &stdout, &stderr
}

func startAttachableBackend(t *testing.T, token string) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.Listener.Addr().(*net.TCPAddr).Port
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

func TestAttachAcceptsAuthenticatedBackendAcrossPIDNamespace(t *testing.T) {
	const token = "t"
	root := t.TempDir()
	port := startAttachableBackend(t, token)
	writeInstanceFile(t, root, deadPID(t), func(bs *harnessclient.Bootstrap) {
		bs.Port = port
		bs.Token = token
	})

	e, _, _ := testEnv(t.TempDir())
	e.instance = root
	client, target, bs, err := e.attach(context.Background())
	if err != nil {
		t.Fatalf("attach across PID namespace: %v", err)
	}
	defer client.Close()
	if target.ID != instanceinfo.ID(root) {
		t.Fatalf("target id = %s, want %s", target.ID, instanceinfo.ID(root))
	}
	if bs.PID == os.Getpid() || instanceinfo.ProcessAlive(bs.PID) {
		t.Fatalf("test bootstrap pid %d unexpectedly resolves in this namespace", bs.PID)
	}
}

func TestAttachRejectsRegistryBootstrapIdentityMismatch(t *testing.T) {
	registry := t.TempDir()
	root := t.TempDir()
	id := seedInstance(t, registry, root, os.Getpid())
	writeInstanceFile(t, root, os.Getpid(), func(bs *harnessclient.Bootstrap) {
		bs.ID = id
		bs.IdentityVersion = instanceinfo.IdentityVersion
		bs.BootNonce = "new-boot"
	})
	// The registry row has no nonce, which models a stale pre-identity row.
	// A current bootstrap must not be attached through it.
	e, _, _ := testEnv(registry)
	e.instance = root
	_, _, _, err := e.attach(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("attach error = %v, want identity mismatch", err)
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
	process, err := instanceinfo.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	identity := instanceinfo.Identity{
		IdentityVersion:  instanceinfo.IdentityVersion,
		ID:               instanceinfo.ID(root),
		Mode:             instanceinfo.ModeHarness,
		BootNonce:        "test-boot",
		ProcessStartTime: process.StartTime,
		ExecutablePath:   process.Executable,
		PIDNamespace:     process.Namespace,
		StartedAt:        "2026-08-26T00:00:00Z",
	}
	seedInstance(t, registry, root, os.Getpid(), func(row *instanceinfo.Row) {
		row.Identity = identity
	})
	writeInstanceFile(t, root, os.Getpid(), func(bs *harnessclient.Bootstrap) {
		bs.Identity = identity
	})

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

func TestDownTreatsAuthenticatedLeaderExitAsTreeReconciliation(t *testing.T) {
	pid := deadPID(t)
	err := reconcileStoppedTree(context.Background(), pid, harnessclient.Bootstrap{Identity: instanceinfo.Identity{
		ProcessStartTime: "already-exited",
		ExecutablePath:   filepath.Join(t.TempDir(), "agent-overflow"),
		PIDNamespace:     instanceinfo.CurrentPIDNamespace(),
	}})
	if err != nil {
		t.Fatalf("reconcile stopped tree: %v", err)
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

// seedRow writes ONLY the registry row, for the resolution cases that care
// about what the registry says rather than what is on disk under the root.
func seedRow(t *testing.T, registryDir, dataRoot string, pid int) string {
	t.Helper()
	id := instanceinfo.ID(dataRoot)
	row := instanceinfo.Row{
		Identity: instanceinfo.Identity{ID: id, Mode: instanceinfo.ModeHarness, StartedAt: "2026-08-26T00:00:00Z"},
		PID:      pid,
		DataRoot: dataRoot,
		DataDir:  filepath.Join(dataRoot, "agent-overflow"),
	}
	if err := instanceinfo.WriteIn(registryDir, row); err != nil {
		t.Fatal(err)
	}
	return id
}

// The git-style convenience: four hex characters is enough when only one
// id starts with them.
func TestResolveTargetAcceptsAUniqueInstanceIDPrefix(t *testing.T) {
	registry := t.TempDir()
	wanted := seedInstance(t, registry, t.TempDir(), os.Getpid())
	other := seedInstance(t, registry, t.TempDir(), os.Getpid())
	if other[:minIDPrefix] == wanted[:minIDPrefix] {
		t.Skip("the two temp roots hashed to a shared prefix")
	}

	e, _, _ := testEnv(registry)
	e.instance = wanted[:minIDPrefix]
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatalf("resolve by prefix: %v", err)
	}
	if got.ID != wanted {
		t.Fatalf("resolved %s, want %s", got.ID, wanted)
	}
}

func TestResolveTargetRefusesAnAmbiguousIDPrefixWithExitTwo(t *testing.T) {
	registry := t.TempDir()
	// Hand-written ids sharing a prefix: the temp-root hashes cannot be
	// steered into a collision on demand.
	for _, id := range []string{"abcd1234", "abcd5678"} {
		row := instanceinfo.Row{
			Identity: instanceinfo.Identity{ID: id, Mode: instanceinfo.ModeHarness, StartedAt: "2026-08-26T00:00:00Z"},
			PID:      os.Getpid(),
			DataRoot: filepath.Join(t.TempDir(), id),
		}
		if err := instanceinfo.WriteIn(registry, row); err != nil {
			t.Fatal(err)
		}
	}

	e, _, _ := testEnv(registry)
	e.instance = "abcd"
	_, err := e.resolveTarget()
	if err == nil {
		t.Fatal("an ambiguous prefix resolved")
	}
	if code := exitCodeOf(t, err); code != exitUsage {
		t.Fatalf("exit code = %d, want %d (under-specified invocation, not a refusal)", code, exitUsage)
	}
	for _, id := range []string{"abcd1234", "abcd5678"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error omits candidate %s: %v", id, err)
		}
	}
}

// Two live instances is ambiguity, and ambiguity is a WRONG INVOCATION —
// exit 2, so a script can tell it from "the harness refused".
func TestResolveTargetAmbiguityExitsTwo(t *testing.T) {
	registry := t.TempDir()
	seedInstance(t, registry, t.TempDir(), os.Getpid())
	seedInstance(t, registry, t.TempDir(), os.Getpid())

	e, _, _ := testEnv(registry)
	_, err := e.resolveTarget()
	if err == nil {
		t.Fatal("want an ambiguity error")
	}
	if code := exitCodeOf(t, err); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

// A developer with a soak in one worktree and a harness in this one means
// THIS one every time. The default root is a derivation, not a guess.
func TestResolveTargetPrefersThisWorktreesOwnInstance(t *testing.T) {
	registry := t.TempDir()
	mine := seedRow(t, registry, instanceinfo.DefaultDataRoot(), os.Getpid())
	theirs := seedInstance(t, registry, t.TempDir(), os.Getpid())

	e, _, _ := testEnv(registry)
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatalf("two live rows, one of them this worktree's: %v", err)
	}
	if got.ID != mine {
		t.Fatalf("resolved %s, want this worktree's own instance %s (the other is %s)", got.ID, mine, theirs)
	}
}

// $AO_HARNESS_INSTANCE is the default for --instance: an agent driving one
// instance for a session exports it once instead of threading the flag
// through every invocation.
func TestInstanceEnvIsTheDefaultForTheFlag(t *testing.T) {
	registry := t.TempDir()
	wanted := seedInstance(t, registry, t.TempDir(), os.Getpid())
	seedInstance(t, registry, t.TempDir(), os.Getpid())

	t.Setenv(instanceEnv, wanted)
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	e.registryDir = registry
	got, err := e.resolveTarget()
	if err != nil {
		t.Fatalf("resolve with $%s set: %v", instanceEnv, err)
	}
	if got.ID != wanted {
		t.Fatalf("resolved %s, want %s", got.ID, wanted)
	}
}

func TestLooksLikeIDPrefixIsShapeOnly(t *testing.T) {
	for _, tc := range []struct {
		selector string
		want     bool
	}{
		{"abcd", true},
		{"deadbeef", true},
		{"abc", false},         // shorter than minIDPrefix: still a path
		{"abcd12345", false},   // longer than an id
		{"abcz", false},        // not hex
		{"CAFEBABE", false},    // ids are lower-case hex
		{"./abcd", false},      // path punctuation
		{"abcd.json", false},   // a file
		{"/tmp/ao-run", false}, // an outright path
		{"", false},
	} {
		if got := looksLikeIDPrefix(tc.selector); got != tc.want {
			t.Errorf("looksLikeIDPrefix(%q) = %v, want %v", tc.selector, got, tc.want)
		}
	}
}

func TestValidateTargetPathsRejectsRegistryPathEscape(t *testing.T) {
	root := t.TempDir()
	if err := validateTargetPaths(root, filepath.Join(root, "elsewhere")); err == nil {
		t.Fatal("accepted registry data directory outside the selected root")
	}
}

func TestValidateTargetPathsRejectsSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateTargetPaths(link, filepath.Join(link, appDataDirName)); err == nil {
		t.Fatal("accepted registry target through a symlinked data root")
	}
}
