package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/ownedtemp"
)

// This test binary is also the go command the runs under test start, and
// the command itself for the runs that replace their process.
const (
	// fakeEnv makes the binary a fake go command: it logs each `go test`
	// to the file fakeEnv names and plays it as the fake* variables say.
	fakeEnv = "AO_GOTEST_FAKE_LOG"
	// fakeFailEnv is a test name whose bucket exits 3, or "whole" for the
	// unsplit run.
	fakeFailEnv = "AO_GOTEST_FAKE_FAIL"
	// fakeListEnv is the exit status of the list step.
	fakeListEnv = "AO_GOTEST_FAKE_LIST"
	// fakeStaleEnv is a test name whose bucket replaces its -o link, as the
	// go command does when the binary is stale.
	fakeStaleEnv = "AO_GOTEST_FAKE_STALE"
	// fakeHoldEnv makes every run but the list step write a line and wait
	// for a signal.
	fakeHoldEnv = "AO_GOTEST_FAKE_HOLD"
	// fakeChildEnv names a directory: every run but the list step starts
	// children (fakeChild) that record themselves there, and leaves them
	// running when it exits, as the go command leaves a test's processes.
	fakeChildEnv = "AO_GOTEST_FAKE_CHILD"
	// childEnv makes the binary such a child, recording itself in the
	// directory it names, and childIgnoreEnv makes it ignore SIGTERM.
	childEnv       = "AO_GOTEST_CHILD"
	childIgnoreEnv = "AO_GOTEST_CHILD_IGNORE"
	// graceEnv sets a leader's endGrace.
	graceEnv = "AO_GOTEST_GRACE"
	// proxyEnv makes the binary the go command it names, after the first
	// bucket to start writes proxyEditEnv's file (see proxyGo).
	proxyEnv     = "AO_GOTEST_PROXY"
	proxyEditEnv = "AO_GOTEST_PROXY_EDIT"
	// mainEnv makes the binary this command.
	mainEnv = "AO_GOTEST_MAIN"
	// orphanEnv makes the binary a run killed after it made its directory.
	orphanEnv = "AO_GOTEST_ORPHAN"
	// markEnv is a variable the fake logs, to show what environment it got.
	markEnv = "AO_GOTEST_MARK"
)

const fakeLines = 40

// fakeNames are the tests the fake's list step prints.
var fakeNames = []string{"TestA", "TestAB", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG", "TestH", "TestI", "TestJ", "TestK"}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == leaderArg {
		if grace := os.Getenv(graceEnv); grace != "" {
			endGrace = must(time.ParseDuration(grace))
		}
		os.Exit(lead(os.Args[2:], os.Stdin, os.Stderr))
	}
	if dir := os.Getenv(childEnv); dir != "" {
		os.Exit(fakeChild(dir))
	}
	// The go command's first argument is its subcommand; this command's is
	// the go command.
	if len(os.Args) > 1 && os.Args[1] == "test" {
		if log := os.Getenv(fakeEnv); log != "" {
			os.Exit(fakeGo(log, os.Args[1:]))
		}
		if real := os.Getenv(proxyEnv); real != "" {
			os.Exit(proxyGo(real, os.Args[1:]))
		}
	}
	if os.Getenv(orphanEnv) != "" {
		if _, _, err := ownedtemp.Make(os.Getenv("GOTMPDIR"), runDirPrefix); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(mainEnv) != "" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func appendLog(log, line string) error {
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	return errors.Join(err, f.Close())
}

// flagValue is the argument after name in args.
func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// runNames are the names a bucket's -run pattern selects.
func runNames(pattern string) []string {
	return strings.Split(strings.TrimSuffix(strings.TrimPrefix(pattern, "^(?:"), ")$"), "|")
}

func fakeGo(log string, args []string) int {
	if err := appendLog(log, "go "+strings.Join(args, " ")+"|"+os.Getenv(markEnv)); err != nil {
		return 9
	}
	if slices.Contains(args, "-list") {
		if status, _ := strconv.Atoi(os.Getenv(fakeListEnv)); status != 0 {
			fmt.Fprintln(os.Stderr, "list failed: a build error")
			return status
		}
		// A list step takes a while, so a bucket started beside it would
		// start before it ends.
		time.Sleep(50 * time.Millisecond)
		if err := errors.Join(os.WriteFile(flagValue(args, "-o"), []byte("binary"), 0o755), appendLog(log, "listed "+args[len(args)-1])); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 9
		}
		// go test prints the binary's stderr with its stdout: a TestMain's
		// log comes before the names.
		fmt.Println("2026/11/23 00:00:00 store: applying migration v1: TestA")
		for _, name := range fakeNames {
			fmt.Println(name)
		}
		fmt.Printf("ok  \t%s\t0.001s\n", args[len(args)-1])
		return 0
	}
	name := "whole"
	var names []string
	if pattern := flagValue(args, "-run"); pattern != "" {
		names = runNames(pattern)
		name = "bucket " + names[0]
	}
	if slices.Contains(names, os.Getenv(fakeStaleEnv)) {
		link := flagValue(args, "-o")
		if err := errors.Join(os.Remove(link), os.WriteFile(link, []byte("its own"), 0o755)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 9
		}
	}
	if dir := os.Getenv(fakeChildEnv); dir != "" {
		if err := startChildren(dir); err != nil {
			fmt.Fprintln(os.Stderr, "start children:", err)
			return 9
		}
	}
	if os.Getenv(fakeHoldEnv) != "" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("before hold", name)
		if err := appendLog(log, "held "+name); err != nil {
			return 9
		}
		sig := <-signals
		fmt.Println("got", sig, name)
		if err := appendLog(log, "ended "+name); err != nil {
			return 9
		}
		return 128 + int(sig.(syscall.Signal))
	}
	for i := range fakeLines {
		// Half a line at a time, so a reader that passed writes through
		// as they came would split lines of different processes.
		fmt.Fprintf(os.Stdout, "%s line", name)
		time.Sleep(200 * time.Microsecond)
		fmt.Fprintf(os.Stdout, " %d\n", i)
		fmt.Fprintf(os.Stderr, "%s err %d\n", name, i)
	}
	if fail := os.Getenv(fakeFailEnv); fail == "whole" && names == nil || slices.Contains(names, fail) {
		return 3
	}
	return 0
}

// startChildren starts the children of a fake run, each a fakeChild
// recording itself in dir, and returns once each has: one in this
// process's group and, on Linux, one in a group of its own, as a test
// starts a provider, which only a subreaper finds once this process exits.
func startChildren(dir string) error {
	own := []bool{false}
	if runtime.GOOS == "linux" {
		own = append(own, true)
	}
	for _, group := range own {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), childEnv+"="+dir)
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: group}
		if err := child.Start(); err != nil {
			return err
		}
		started := filepath.Join(dir, strconv.Itoa(child.Process.Pid)+".started")
		deadline := time.Now().Add(time.Minute)
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("child %d never started", child.Process.Pid)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

// fakeChild records that it started in dir, and when SIGTERM comes, unless
// childIgnoreEnv is set, takes a while to clean up, records that it
// exited, and exits.
func fakeChild(dir string) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	marker := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(marker+".started", nil, 0o600); err != nil {
		return 9
	}
	for range signals {
		if os.Getenv(childIgnoreEnv) == "" {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(marker+".exited", nil, 0o600); err != nil {
		return 9
	}
	return 128 + int(syscall.SIGTERM)
}

// recorded reads what the children recorded in dir: each one's pid, and
// whether it recorded that it exited.
func recorded(t *testing.T, dir string) map[int]bool {
	t.Helper()
	started, err := filepath.Glob(filepath.Join(dir, "*.started"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, path := range started {
		base := strings.TrimSuffix(path, ".started")
		pid, err := strconv.Atoi(filepath.Base(base))
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(base + ".exited")
		got[pid] = err == nil
	}
	return got
}

// checkChildrenGone checks that at least least children recorded
// themselves in dir, that each is gone, and, when exited, that each
// recorded that it exited after its SIGTERM.
func checkChildrenGone(t *testing.T, dir string, least int, exited bool) {
	t.Helper()
	got := recorded(t, dir)
	if len(got) < least {
		t.Fatalf("%d children started; want at least %d", len(got), least)
	}
	for pid, done := range got {
		if exited && !done {
			t.Errorf("child %d had not exited after its SIGTERM", pid)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("child %d is still running (%v)", pid, err)
		}
	}
}

// childrenPerRun is how many children startChildren starts.
func childrenPerRun() int {
	if runtime.GOOS == "linux" {
		return 2
	}
	return 1
}

// proxyGo runs the go command real with args. The first bucket to start
// first records the SHA-256 of the binary its -o link names in path.sum
// and writes the file proxyEditEnv names as "path=content": after the list
// step and before the go command reads the package.
func proxyGo(real string, args []string) int {
	if edit := os.Getenv(proxyEditEnv); edit != "" && flagValue(args, "-run") != "" {
		path, content, _ := strings.Cut(edit, "=")
		// O_EXCL on a marker makes exactly one bucket the first.
		if marker, err := os.OpenFile(path+".edited", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			binary, err := os.ReadFile(flagValue(args, "-o"))
			sum := sha256.Sum256(binary)
			if err := errors.Join(err, marker.Close(),
				os.WriteFile(path+".sum", []byte(hex.EncodeToString(sum[:])), 0o600),
				os.WriteFile(path, []byte(content), 0o644)); err != nil {
				fmt.Fprintln(os.Stderr, "proxy:", err)
				return 9
			}
		}
	}
	err := syscall.Exec(real, append([]string{"go"}, args...), os.Environ())
	fmt.Fprintln(os.Stderr, "proxy: exec", real, err)
	return 9
}

func TestParseArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      string
		shardable bool
		packages  string
	}{
		{"", true, ""},
		{"./a ./b", true, "./a ./b"},
		{"-timeout 30m -count=1 ./a", true, "./a"},
		{"-timeout 30m -count 1 -v ./a ./b", true, "./a ./b"},
		{"./a -count 1 ./b --v", true, "./a ./b"},
		{"--count=2 --short", true, ""},
		{"-tags noremote -count=1 ./a", true, "./a"},
		{"-tags=noremote -vet=off -short -shuffle on -skip TestX", true, ""},
		{"-failfast ./a", false, ""},
		{"-json ./a", false, ""},
		{"-p 4 ./a", false, ""},
		{"-p=1", false, ""},
		{"-timeout 30m -run TestX -count=1", false, ""},
		{"-run=TestX", false, ""},
		{"-list .", false, ""},
		{"-bench . -benchtime 1x", false, ""},
		{"-fuzz FuzzX", false, ""},
		{"-parallel 4", false, ""},
		{"-cpu 1,2", false, ""},
		{"-race", false, ""},
		{"-cover", false, ""},
		{"-coverprofile c.out", false, ""},
		{"-cpuprofile cpu.out", false, ""},
		{"-memprofile mem.out", false, ""},
		{"-trace trace.out", false, ""},
		{"-c", false, ""},
		{"-o app.test", false, ""},
		{"-exec echo", false, ""},
		{"./a -args -x", false, ""},
		{"-count", false, ""},
	} {
		inv, err := parseArgs(append([]string{"go", "test"}, strings.Fields(tc.args)...))
		if err != nil {
			t.Fatalf("parseArgs(go test %s): %v", tc.args, err)
		}
		if inv.shardable != tc.shardable {
			t.Errorf("go test %s: shardable = %v; want %v", tc.args, inv.shardable, tc.shardable)
		}
		if got := strings.Join(inv.packages, " "); tc.shardable && got != tc.packages {
			t.Errorf("go test %s: packages %q; want %q", tc.args, got, tc.packages)
		}
	}
	for _, argv := range [][]string{nil, {"go"}, {"go", "vet", "./a"}, {"./a", "--", "-count=1"}} {
		if _, err := parseArgs(argv); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("parseArgs(%q) = %v; want the usage error", argv, err)
		}
	}
}

func TestParseSplits(t *testing.T) {
	t.Parallel()
	splits, rest, err := parseSplits([]string{"-split=example/a=512,1024", "-split", "example/b=64,32", "go", "test", "-split=x=1,1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []split{{"example/a", 512 << 20, 1024 << 20}, {"example/b", 64 << 20, 32 << 20}}
	if !slices.Equal(splits, want) || !slices.Equal(rest, []string{"go", "test", "-split=x=1,1"}) {
		t.Fatalf("splits %v, rest %q; want %v and the go command line", splits, rest, want)
	}
	for _, bad := range []string{"example/a", "example/a=", "=512,1", "example/a=512", "example/a=0,1", "example/a=1,0", "example/a=1.5,1", "example/a=-1,1", "example/a=1,2,3"} {
		if _, _, err := parseSplits([]string{"-split=" + bad, "go", "test"}); err == nil {
			t.Errorf("-split=%s parsed; want an error", bad)
		}
	}
}

// TestGOFLAGSDecidesASplitAsTheCommandLineDoes: GOFLAGS splits a run only
// when every flag it sets is one a bucket keeps or a build flag that
// changes nothing but the binary, as make's -trimpath; a field the go
// command would refuse leaves the run whole for it to report.
func TestGOFLAGSDecidesASplitAsTheCommandLineDoes(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"":                                  true,
		"-trimpath":                         true,
		" -trimpath\t-mod=mod -modcacherw ": true,
		"-buildvcs=false -count=1 -v":       true,
		"--count=1 -test.timeout=5m":        true,
		"'-tags=a b' -short":                true,
		"-run=^TestB$":                      false,
		"-test.run=TestB":                   false,
		"-trimpath -list=.":                 false,
		"-json":                             false,
		"-failfast":                         false,
		"-p=1":                              false,
		"-coverprofile=c.out":               false,
		"-cover":                            false,
		"-race":                             false,
		"'-ldflags=-s -w'":                  false,
		"-tags=a 'unterminated":             false,
		"trimpath":                          false,
	} {
		if got := goflagsSplit(value); got != want {
			t.Errorf("GOFLAGS=%q splits %v; want %v", value, got, want)
		}
	}
}

// TestReservationUnitsCapAtOnePackagesBuckets: a run reserves at most a
// unit per core and at most one package's buckets, the largest first, the
// first raised to the largest list step.
func TestReservationUnitsCapAtOnePackagesBuckets(t *testing.T) {
	t.Parallel()
	app, store := split{"example/app", 1664 << 20, 2432 << 20}, split{"example/store", 640 << 20, 640 << 20}
	repeat := func(first, rest uint64, n int) []uint64 {
		return append([]uint64{first << 20}, slices.Repeat([]uint64{rest << 20}, n-1)...)
	}
	for _, tc := range []struct {
		declared []split
		cores    int
		want     []uint64
	}{
		{[]split{app, store}, 24, repeat(2432, 1664, 8)},
		{[]split{store, app}, 24, repeat(2432, 1664, 8)},
		{[]split{app, store}, 4, repeat(2432, 1664, 4)},
		{[]split{app, store}, 1, repeat(2432, 1664, 1)},
		{[]split{store}, 24, repeat(640, 640, 8)},
		{[]split{{"example/small", 100 << 20, 300 << 20}}, 2, repeat(300, 100, 2)},
	} {
		if got := reservationUnits(tc.declared, tc.cores); !slices.Equal(got, tc.want) {
			t.Errorf("%v on %d cores: units %v; want %v", tc.declared, tc.cores, got, tc.want)
		}
	}
}

// fixedGovernor is a governor whose host has available bytes, with a
// runner that asks it every 10ms, and the request a run makes.
func fixedGovernor(t *testing.T, available uint64) (*fixedMemory, *governor.Manager, *runner, *lockedWriter, governor.Request) {
	t.Helper()
	memory := &fixedMemory{available: available}
	m, err := governor.New(governor.Options{Dir: t.TempDir(), AvailableFloorBytes: 1 << 30, Memory: memory})
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedWriter{}
	r := must(newRunner("go", &lockedWriter{}, stderr))
	t.Cleanup(r.close)
	r.admitPoll = 10 * time.Millisecond
	return memory, m, r, stderr, governor.Request{RunID: "go-test", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()}
}

// TestAZeroGrantWaitsUntilAHolderReleases has another run hold all but
// half a unit above the floor: the run says once why it waits and starts
// no bucket until that run releases, then runs as many as fit.
func TestAZeroGrantWaitsUntilAHolderReleases(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	_, m, r, stderr, req := fixedGovernor(t, floor+5*unit/2)
	holder, err := m.Reserve(governor.Request{RunID: "holder", Worktree: "/elsewhere", DataRoot: t.TempDir(), OwnerPID: os.Getpid(), CeilingBytes: 2 * unit})
	if err != nil {
		t.Fatal(err)
	}
	type grant struct {
		w       int
		release func() error
		err     error
	}
	granted := make(chan grant, 1)
	go func() {
		w, release, err := r.reserve(m, req, []uint64{unit, unit, unit, unit})
		granted <- grant{w, release, err}
	}()
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(stderr.String(), "waiting for memory") {
		if time.Now().After(deadline) {
			t.Fatal("the run never said it was waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case g := <-granted:
		t.Fatalf("granted %d (%v) while nothing fits; want a wait", g.w, g.err)
	case <-time.After(100 * time.Millisecond):
	}
	want := "ao-gotest: waiting for memory for the buckets: one bucket needs 100 MiB and 50 MiB is free: 1274 MiB available less the 1024 MiB floor the host keeps and the 200 MiB other runs reserved (holder in /elsewhere holds 200 MiB)\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr %q; want %q, once", got, want)
	}
	if err := m.Release(holder); err != nil {
		t.Fatal(err)
	}
	g := <-granted
	if g.err != nil || g.w != 2 {
		t.Fatalf("granted %d, %v once the holder released; want the 2 units that fit", g.w, g.err)
	}
	if err := g.release(); err != nil {
		t.Fatal(err)
	}
}

// TestARefusalPastItsBoundFailsWithTheReason: a run that nothing frees
// for fails with the reason once its wait is over, holding no lease.
func TestARefusalPastItsBoundFailsWithTheReason(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	_, m, r, stderr, req := fixedGovernor(t, floor+unit/2)
	r.admitWait = 50 * time.Millisecond
	w, _, err := r.reserve(m, req, []uint64{unit})
	if w != 0 || err == nil || !strings.HasPrefix(err.Error(), "no memory for the buckets after 50ms: one bucket needs 100 MiB and 50 MiB is free") {
		t.Fatalf("reserve = %d, %v; want the refusal", w, err)
	}
	if got := strings.Count(stderr.String(), "waiting for memory"); got != 1 {
		t.Fatalf("said it was waiting %d times; want once:\n%s", got, stderr.String())
	}
	if snap, err := m.Snapshot(); err != nil || len(snap.Leases) != 0 {
		t.Fatalf("leases %v, %v; want none", snap.Leases, err)
	}
}

// TestASignalEndsTheWaitForMemory: a signal while the run waits ends the
// wait at once.
func TestASignalEndsTheWaitForMemory(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	_, m, r, _, req := fixedGovernor(t, floor)
	done := make(chan error, 1)
	go func() {
		_, _, err := r.reserve(m, req, []uint64{unit})
		done <- err
	}()
	r.signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if !errors.Is(err, errStopped) {
			t.Fatalf("reserve = %v; want it stopped", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the wait went on after the signal")
	}
}

type fixedMemory struct{ available uint64 }

func (f *fixedMemory) AvailableMemory() (uint64, error) { return f.available, nil }

func fakeRunner(t *testing.T, env ...string) (r *runner, stdout, stderr *lockedWriter, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "calls")
	t.Setenv(fakeEnv, log)
	t.Setenv(markEnv, "")
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	stdout, stderr = &lockedWriter{}, &lockedWriter{}
	r = must(newRunner(os.Args[0], stdout, stderr))
	t.Cleanup(r.close)
	return r, stdout, stderr, log
}

// calls are the fake's logged calls, sorted, without the hold lines.
func calls(t *testing.T, log string) []string {
	t.Helper()
	var got []string
	for line := range strings.Lines(readFile(t, log)) {
		if !strings.HasPrefix(line, "held ") && !strings.HasPrefix(line, "ended ") && !strings.HasPrefix(line, "listed ") {
			got = append(got, strings.TrimSuffix(line, "\n"))
		}
	}
	slices.Sort(got)
	return got
}

func testPlan(t *testing.T, dir string, unit uint64) runPlan {
	t.Helper()
	inv, err := parseArgs([]string{"go", "test", "-timeout", "30m", "-count=1", "-tags", "x", "example/whole", "example/split"})
	if err != nil {
		t.Fatal(err)
	}
	return plan(inv, []split{{"example/split", unit, unit}}, []string{"example/whole"}, dir)
}

// wantCalls are the calls the plan of testPlan makes: the list step, one
// call per bucket with a link of its own and exactly its names, and the
// unsplit run. Every listed name is in one bucket.
func wantCalls(dir string) []string {
	binary := filepath.Join(dir, "0-split.test")
	want := []string{
		"go test -tags x -o " + binary + " -list . example/split|",
		"go test -timeout 30m -count=1 -tags x example/whole|",
	}
	byBucket := map[int][]string{}
	for _, name := range fakeNames {
		byBucket[bucketOf(name)] = append(byBucket[bucketOf(name)], name)
	}
	for i, names := range byBucket {
		link := filepath.Join(dir, "0-split-"+strconv.Itoa(i)+".test")
		want = append(want, "go test -timeout 30m -count=1 -tags x -o "+link+" -parallel 1 -run ^(?:"+strings.Join(names, "|")+")$ example/split|")
	}
	slices.Sort(want)
	return want
}

// TestASplitRunPrintsWholeLinesAndEachBucketWhole runs a split package's
// buckets and an unsplit run at once and checks the calls each made, that
// no line of one is cut by another's, and that each bucket's output is
// one block under its header.
func TestASplitRunPrintsWholeLinesAndEachBucketWhole(t *testing.T) {
	r, stdout, stderr, log := fakeRunner(t, fakeFailEnv+"=TestC")
	dir := t.TempDir()
	if status := r.run(testPlan(t, dir, 1<<40), 3); status != 3 {
		t.Errorf("status %d; want 3, the failed bucket's", status)
	}
	if got, want := calls(t, log), wantCalls(dir); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	blocks := len(wantCalls(dir)) - 2
	checkBlocks(t, "stdout", stdout.String(), "line", blocks, true)
	checkBlocks(t, "stderr", stderr.String(), "err", blocks, false)
	if info, err := os.Stat(filepath.Join(dir, "0-split.test")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("the shared binary: %v, %v; want it read-only", info, err)
	}
}

// TestEveryListStepRunsBeforeAnyBucket splits two packages: both list
// steps end, one after the other, before the first bucket starts, so no
// link runs beside a bucket the reservation counts.
func TestEveryListStepRunsBeforeAnyBucket(t *testing.T) {
	r, _, _, log := fakeRunner(t)
	inv, err := parseArgs([]string{"go", "test", "-count=1", "example/split", "example/other"})
	if err != nil {
		t.Fatal(err)
	}
	p := plan(inv, []split{{"example/split", 1 << 40, 1 << 40}, {"example/other", 1 << 40, 1 << 40}}, nil, t.TempDir())
	if status := r.run(p, 8); status != 0 {
		t.Fatalf("status %d", status)
	}
	var order []string
	for line := range strings.Lines(readFile(t, log)) {
		switch {
		case strings.HasPrefix(line, "listed "):
			order = append(order, strings.TrimSpace(line))
		case strings.Contains(line, " -list . "):
			order = append(order, "list "+strings.Fields(line)[len(strings.Fields(line))-1])
		case strings.Contains(line, " -run "):
			if len(order) == 0 || order[len(order)-1] != "bucket" {
				order = append(order, "bucket")
			}
		}
	}
	want := []string{"list example/split|", "listed example/split", "list example/other|", "listed example/other", "bucket"}
	if !slices.Equal(order, want) {
		t.Fatalf("order %q; want %q\n%s", order, want, readFile(t, log))
	}
}

// checkBlocks checks that every line is one the fake wrote whole, that the
// unsplit run's lines came in order, and that each bucket's lines came as
// one block of consecutive lines, after its header when headed.
func checkBlocks(t *testing.T, stream, out, word string, blocks int, headed bool) {
	t.Helper()
	header := regexp.MustCompile(`^ao-gotest: example/split bucket \d of 8$`)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	whole := 0
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		if header.MatchString(lines[i]) != (headed && strings.HasPrefix(lines[i], "ao-gotest: ")) {
			t.Fatalf("%s: line %q; want a header only before a bucket's block in stdout", stream, lines[i])
		}
		if headed {
			if !header.MatchString(lines[i]) {
				if lines[i] != fmt.Sprintf("whole %s %d", word, whole) {
					t.Fatalf("%s: line %q; want a header or the unsplit run's next line", stream, lines[i])
				}
				whole++
				continue
			}
			i++
		}
		if i >= len(lines) {
			t.Fatalf("%s: a header ends the output", stream)
		}
		owner, _, ok := strings.Cut(lines[i], " "+word+" ")
		if !ok || !strings.HasPrefix(owner, "bucket ") {
			if headed || lines[i] != fmt.Sprintf("whole %s %d", word, whole) {
				t.Fatalf("%s: line %q; want a bucket's first line or the unsplit run's next", stream, lines[i])
			}
			whole++
			continue
		}
		for n := range fakeLines {
			want := fmt.Sprintf("%s %s %d", owner, word, n)
			if i >= len(lines) || lines[i] != want {
				t.Fatalf("%s: in the block of %s, line %d is %q; want %q", stream, owner, n, lines[min(i, len(lines)-1)], want)
			}
			i++
		}
		i--
		seen[owner] = true
	}
	if whole != fakeLines || len(seen) != blocks {
		t.Errorf("%s: %d lines of the unsplit run and the blocks of %v; want %d and %d blocks", stream, whole, seen, fakeLines, blocks)
	}
}

// TestAFailedListPrintsItsOutputOnceAndRunsNoBucket checks that a split
// package whose list step fails reports that once, with its status, and
// starts no bucket.
func TestAFailedListPrintsItsOutputOnceAndRunsNoBucket(t *testing.T) {
	r, _, stderr, log := fakeRunner(t, fakeListEnv+"=2")
	dir := t.TempDir()
	if status := r.run(testPlan(t, dir, 1<<40), 3); status != 2 {
		t.Errorf("status %d; want 2, the list step's", status)
	}
	want := []string{
		"go test -tags x -o " + filepath.Join(dir, "0-split.test") + " -list . example/split|",
		"go test -timeout 30m -count=1 -tags x example/whole|",
	}
	if got := calls(t, log); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if out := stderr.String(); strings.Count(out, "list failed: a build error\n") != 1 || !strings.Contains(out, "ao-gotest: list the tests of example/split: exit status 2\n") {
		t.Fatalf("stderr:\n%s\nwant the list step's failure once, named", out)
	}
}

// TestABucketThatBuildsItsOwnBinaryFailsAndSaysWhy has one bucket replace
// its link, as the go command does when the package changed after the
// list step: that bucket fails with the reason, and the others pass.
func TestABucketThatBuildsItsOwnBinaryFailsAndSaysWhy(t *testing.T) {
	r, _, stderr, _ := fakeRunner(t, fakeStaleEnv+"=TestC")
	if status := r.run(testPlan(t, t.TempDir(), 1<<40), 3); status != 1 {
		t.Errorf("status %d; want 1", status)
	}
	label := fmt.Sprintf("example/split bucket %d of 8", bucketOf("TestC"))
	want := "ao-gotest: " + label + " built its own test binary: example/split or a package it imports changed after this run listed its tests"
	if out := stderr.String(); strings.Count(out, " built its own test binary") != 1 || !strings.Contains(out, want) {
		t.Fatalf("stderr:\n%s\nwant one bucket named: %s", out, want)
	}
}

// TestABucketOverItsUnitIsNamedAtTheEnd declares a unit below what any
// process holds, and then one above it.
func TestABucketOverItsUnitIsNamedAtTheEnd(t *testing.T) {
	for _, tc := range []struct {
		unit  uint64
		named bool
	}{{1 << 20, true}, {1 << 40, false}} {
		r, _, stderr, _ := fakeRunner(t)
		if status := r.run(testPlan(t, t.TempDir(), tc.unit), 3); status != 0 {
			t.Fatalf("status %d", status)
		}
		r.printNotes()
		lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
		over := fmt.Sprintf(" MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", tc.unit>>20)
		bucket := len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "ao-gotest: example/split: a bucket's largest process reached ") && strings.HasSuffix(lines[len(lines)-1], over)
		list := len(lines) > 1 && strings.HasPrefix(lines[len(lines)-2], "ao-gotest: example/split: its list step's largest process reached ") && strings.HasSuffix(lines[len(lines)-2], over)
		if bucket != tc.named || list != tc.named {
			t.Errorf("unit %d MiB: last lines %q; bucket and list step named %v and %v, want %v", tc.unit>>20, lines[max(0, len(lines)-2):], bucket, list, tc.named)
		}
	}
}

// TestASignalReachesEveryRunningProcessAndStartsNoMore holds every run
// open, each with children that outlive it unless they are signaled too,
// signals the runner, and checks that each running `go test` and every
// child got the signal and ended with it, that the run ended only after
// they had, and that no queued bucket started after it. While the
// buckets hold, what each wrote is in its file in the run's directory.
func TestASignalReachesEveryRunningProcessAndStartsNoMore(t *testing.T) {
	kids := t.TempDir()
	r, stdout, _, log := fakeRunner(t, fakeHoldEnv+"=1", fakeChildEnv+"="+kids)
	dir := t.TempDir()
	done := make(chan int)
	// Two slots for more buckets: the rest are queued when the signal
	// comes.
	go func() { done <- r.run(testPlan(t, dir, 1<<40), 2) }()
	// The unsplit run and the buckets in the two slots hold, each once its
	// children and its signal handler are set.
	deadline := time.Now().Add(time.Minute)
	for strings.Count(readFile(t, log), "held ") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the runs never held:\n%s", readFile(t, log))
		}
		time.Sleep(5 * time.Millisecond)
	}
	outputs, err := filepath.Glob(filepath.Join(dir, "0-split-*.test.out"))
	if err != nil || len(outputs) != 2 {
		t.Fatalf("output files %v, %v; want the two running buckets'", outputs, err)
	}
	for _, path := range outputs {
		if got := readFile(t, path); !strings.HasPrefix(got, "before hold bucket ") {
			t.Fatalf("%s holds %q; want what its bucket wrote so far", path, got)
		}
	}
	r.signal(syscall.SIGTERM)
	var status int
	select {
	case status = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the runner did not end after the signal")
	}
	checkChildrenGone(t, kids, 3*childrenPerRun(), true)
	if got := len(recorded(t, kids)); got != 3*childrenPerRun() {
		t.Errorf("%d children started; want those of the 3 runs that held", got)
	}
	if status != 128+int(syscall.SIGTERM) {
		t.Errorf("status %d; want %d", status, 128+int(syscall.SIGTERM))
	}
	if got := strings.Count(stdout.String(), "got terminated"); got != 3 {
		t.Errorf("%d runs got the signal; want 3\n%s", got, stdout.String())
	}
	if got := calls(t, log); len(got) != 4 {
		t.Errorf("calls:\n%s\nwant the list step, the unsplit run and two buckets", strings.Join(got, "\n"))
	}
}

// TestAGoTestThatLeavesProcessesHasThemEndedAndNamed: a `go test` that
// exits leaving children that ignore SIGTERM has them killed once the
// grace has passed, before its run ends, and they are named.
func TestAGoTestThatLeavesProcessesHasThemEndedAndNamed(t *testing.T) {
	kids := t.TempDir()
	r, _, stderr, _ := fakeRunner(t, fakeChildEnv+"="+kids, childIgnoreEnv+"=1", graceEnv+"=100ms")
	inv, err := parseArgs([]string{"go", "test", "example/whole"})
	if err != nil {
		t.Fatal(err)
	}
	if status := r.run(plan(inv, nil, []string{"example/whole"}, t.TempDir()), 1); status != 0 {
		t.Fatalf("status %d; want the go test's 0", status)
	}
	checkChildrenGone(t, kids, childrenPerRun(), false)
	if out := stderr.String(); !strings.Contains(out, "ao-gotest: the go test left ") || !strings.Contains(out, "which were ended: ") {
		t.Fatalf("stderr:\n%s\nwant the processes it left named", out)
	}
}

// commandEnv is the environment of this command run as `make go-test` runs
// it, with this binary as its go command and the governor in a home of its
// own, and gotmp as GOTMPDIR, and the command line make gives it.
func commandEnv(t *testing.T, gotmp string, args ...string) *exec.Cmd {
	t.Helper()
	bin := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], append([]string{"-split=" + splitPkg + "=1,1", "go", "test"}, args...)...)
	command.Dir = t.TempDir()
	command.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		for _, name := range []string{"PATH", "HOME", "XDG_CACHE_HOME", "GOFLAGS", "GOENV", "GOTMPDIR", "TMPDIR"} {
			if strings.HasPrefix(kv, name+"=") {
				return true
			}
		}
		return false
	}), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+t.TempDir(), "GOENV=off",
		"GOTMPDIR="+gotmp, "TMPDIR="+t.TempDir(), mainEnv+"=1", markEnv+"=kept")
	return command
}

const splitPkg, wholePkg = "example/split", "example/whole"

// leases reads the reservations of the governor in home.
func leases(t *testing.T, home string) []governor.Lease {
	t.Helper()
	m, err := governor.New(governor.Options{Dir: filepath.Join(home, ".cache", "agent-overflow", "harness-governor")})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.Leases
}

func envValue(command *exec.Cmd, name string) string {
	for _, kv := range slices.Backward(command.Env) {
		if value, ok := strings.CutPrefix(kv, name+"="); ok {
			return value
		}
	}
	return ""
}

// TestARunThatSplitsNothingIsTheOneGoTest runs the command for runs it must
// not split and checks that each replaced itself with the `go test` command
// line it was given, in an environment it did not change; and that one
// that splits, under make's GOFLAGS, reserves in its own governor, lists
// the package first into a directory on GOTMPDIR, and leaves no directory
// of its own or of a run that was killed.
func TestARunThatSplitsNothingIsTheOneGoTest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		goflags string
		split   bool
	}{
		{name: "a narrowed run", args: []string{"-timeout", "30m", "-run", "TestX", "-count=1", splitPkg}},
		{name: "a profile", args: []string{"-cpuprofile", "cpu.out", splitPkg}},
		{name: "JSON", args: []string{"-json", splitPkg}},
		{name: "fail fast", args: []string{"-failfast", splitPkg}},
		{name: "a build parallelism", args: []string{"-p", "1", splitPkg}},
		{name: "GOFLAGS narrows the run", args: []string{"-count=1", splitPkg}, goflags: "-trimpath -run=^TestB$"},
		{name: "GOFLAGS asks for JSON", args: []string{"-count=1", splitPkg}, goflags: "-json"},
		{name: "no package is declared", args: []string{"-timeout", "30m", "-count=1", wholePkg}},
		{name: "a package is declared", args: []string{"-timeout", "30m", "-count=1", splitPkg}, goflags: "-trimpath -count=1", split: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			gotmp := t.TempDir()
			command := commandEnv(t, gotmp, tc.args...)
			command.Env = append(command.Env, fakeEnv+"="+log, "GOFLAGS="+tc.goflags)
			orphan := exec.Command(os.Args[0])
			orphan.Env = append(slices.Clone(command.Env), orphanEnv+"=1")
			if out, err := orphan.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := calls(t, log)
			if tc.split {
				binary := regexp.MustCompile(` -o ` + regexp.QuoteMeta(gotmp) + `/` + runDirPrefix + `[^/ ]+/0-split\.test -list \. ` + splitPkg + `\|kept$`)
				if len(got) != 1+len(wantCalls(""))-2 || !slices.ContainsFunc(got, binary.MatchString) || !strings.Contains(string(out), "ao-gotest: "+splitPkg+": 8 buckets each, ") {
					t.Fatalf("calls %q\n%s\nwant the list step into GOTMPDIR, and the buckets", got, out)
				}
				if left, err := filepath.Glob(filepath.Join(gotmp, runDirPrefix+"*")); err != nil || len(left) != 0 {
					t.Fatalf("left %v, %v; want the run's directory and the killed run's removed", left, err)
				}
				if held := leases(t, envValue(command, "HOME")); len(held) != 0 {
					t.Fatalf("leases %v; want the run's released", held)
				}
				return
			}
			// The variable the run was given reaches the one go test.
			want := "go test " + strings.Join(tc.args, " ") + "|kept"
			if len(got) != 1 || got[0] != want {
				t.Fatalf("calls %q; want [%q]", got, want)
			}
		})
	}
}

// TestARunThatIsSignaledOrKilledEndsEveryProcessOfIt holds the buckets of
// a run of the command, with children that outlive their `go test` unless
// they are signaled too. Signaled, the command exits only once each child
// has, and then its reservation and directory are gone. Killed outright,
// every child still gets its SIGTERM.
func TestARunThatIsSignaledOrKilledEndsEveryProcessOfIt(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			kids, gotmp := t.TempDir(), t.TempDir()
			command := commandEnv(t, gotmp, "-count=1", splitPkg)
			command.Env = append(command.Env, fakeEnv+"="+log, fakeHoldEnv+"=1", fakeChildEnv+"="+kids)
			out := &lockedWriter{}
			command.Stdout, command.Stderr = out, out
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			// The run is admitted at least one bucket; each that starts holds.
			deadline := time.Now().Add(time.Minute)
			for strings.Count(readFile(t, log), "held ") < 1 {
				if time.Now().After(deadline) {
					t.Fatalf("no bucket held:\n%s\n%s", readFile(t, log), out.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := command.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-exited:
			case <-time.After(time.Minute):
				t.Fatalf("the command did not end after %v\n%s", sig, out.String())
			}
			if sig == syscall.SIGKILL {
				// Each leader reads the end of its lifeline and ends its tree:
				// every run that held ends, and every child exits and is
				// reaped.
				deadline := time.Now().Add(time.Minute)
				for {
					log := readFile(t, log)
					got := recorded(t, kids)
					gone := !slices.ContainsFunc(slices.Collect(maps.Keys(got)), func(pid int) bool { return syscall.Kill(pid, 0) == nil })
					if gone && strings.Count(log, "ended ") == strings.Count(log, "held ") && !slices.Contains(slices.Collect(maps.Values(got)), false) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("after the command was killed: children %v\n%s", got, log)
					}
					time.Sleep(10 * time.Millisecond)
				}
				checkChildrenGone(t, kids, childrenPerRun(), true)
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
				t.Fatalf("exit %v; want status %d\n%s", err, 128+int(syscall.SIGTERM), out.String())
			}
			checkChildrenGone(t, kids, childrenPerRun(), true)
			if left, err := filepath.Glob(filepath.Join(gotmp, runDirPrefix+"*")); err != nil || len(left) != 0 {
				t.Fatalf("left %v, %v; want the run's directory removed", left, err)
			}
			if held := leases(t, envValue(command, "HOME")); len(held) != 0 {
				t.Fatalf("leases %v; want the run's released", held)
			}
		})
	}
}

// fixture copies testdata/fixture, a module whose package split runs as
// buckets with the real go command, and runs the tests below in it.
func fixture(t *testing.T) (goCommand, dir string) {
	t.Helper()
	goCommand, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/fixture")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	return goCommand, dir
}

// fixturePlan is the plan for `go test -v` of the fixture's package,
// split, with its binaries in a directory of its own.
func fixturePlan(t *testing.T, goCommand string) runPlan {
	t.Helper()
	inv, err := parseArgs([]string{goCommand, "test", "-v", "example.com/fixture/split"})
	if err != nil {
		t.Fatal(err)
	}
	return plan(inv, []split{{"example.com/fixture/split", 1 << 40, 1 << 40}}, nil, t.TempDir())
}

// fixtureRun is one run's output, bucket by bucket.
type fixtureRun struct {
	status int
	// buckets holds each bucket's stdout and stderr by its number.
	buckets map[int]string
	out     string
}

func runFixture(t *testing.T, goCommand string, w int) fixtureRun {
	t.Helper()
	out := &lockedWriter{}
	r := must(newRunner(goCommand, out, out))
	t.Cleanup(r.close)
	status := r.run(fixturePlan(t, goCommand), w)
	run := fixtureRun{status: status, buckets: map[int]string{}, out: out.String()}
	header := regexp.MustCompile(`(?m)^ao-gotest: example.com/fixture/split bucket (\d) of 8\n`)
	marks := header.FindAllStringSubmatchIndex(run.out, -1)
	for i, m := range marks {
		end := len(run.out)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		n, _ := strconv.Atoi(run.out[m[2]:m[3]])
		run.buckets[n] = run.out[m[1]:end]
	}
	if len(run.buckets) < 4 {
		t.Fatalf("%d buckets ran; want the fixture's names in at least four\n%s", len(run.buckets), run.out)
	}
	return run
}

var (
	cachedLine = regexp.MustCompile(`(?m)^ok  \texample.com/fixture/split\t\(cached\)$`)
	// The go command's debug line for a test ID: the binary's ID, then
	// the test ID, which adds the cacheable flags.
	testID = regexp.MustCompile(`(?m)^testcache: example.com/fixture/split: test ID [0-9a-f]+ => ([0-9a-f]+)$`)
	topRun = regexp.MustCompile(`(?m)^=== RUN   ([^/\s]+)$`)
)

// TestBucketsCacheApartAndAcrossWorkerCounts runs the fixture's package as
// buckets five times with the real go command, each run with a binary at a
// path of its own: TestFails fails twice, then passes, at one, two, four,
// one and two buckets at a time. Each bucket has a test ID, and so a cache
// entry, of its own, so the failing bucket reruns while the rest come from
// the cache, and the last two runs are cached whole: a bucket's entry does
// not depend on how many ran at once or where its binary was.
func TestBucketsCacheApartAndAcrossWorkerCounts(t *testing.T) {
	goCommand, _ := fixture(t)
	t.Setenv("GODEBUG", "gocachetest=1")
	failing := bucketOf("TestFails")
	t.Setenv("FIXTURE_FAIL", "1")
	first := runFixture(t, goCommand, 1)
	if first.status != 1 || !strings.Contains(first.buckets[failing], "--- FAIL: TestFails") {
		t.Fatalf("status %d; want 1 with TestFails failing\n%s", first.status, first.out)
	}
	// Every listed name ran once, in one bucket.
	ran := map[string]int{}
	// Each test ID belongs to one bucket.
	ids := map[string]int{}
	for n, out := range first.buckets {
		for _, m := range topRun.FindAllStringSubmatch(out, -1) {
			ran[m[1]]++
		}
		found := testID.FindAllStringSubmatch(out, -1)
		if found == nil {
			t.Fatalf("bucket %d printed no test ID\n%s", n, out)
		}
		for _, m := range found {
			if owner, ok := ids[m[1]]; ok && owner != n {
				t.Fatalf("buckets %d and %d share test ID %s, and so a cache entry\n%s", owner, n, m[1], first.out)
			}
			ids[m[1]] = n
		}
	}
	listed := []string{"ExampleVersion", "FuzzVersion", "TestA", "TestAB", "TestABC", "TestC", "TestD", "TestE", "TestFails"}
	for _, name := range listed {
		if ran[name] != 1 {
			t.Errorf("%s ran %d times; want once (ran %v)", name, ran[name], ran)
		}
	}
	if len(ran) != len(listed) {
		t.Fatalf("ran %v; want only the listed names", ran)
	}
	second := runFixture(t, goCommand, 2)
	for n, out := range second.buckets {
		if cached := cachedLine.MatchString(out); cached == (n == failing) {
			t.Fatalf("second run, bucket %d (cached %v); want only bucket %d, which failed, to run\n%s", n, cached, failing, second.out)
		}
	}
	if second.status != 1 || !strings.Contains(second.buckets[failing], "--- FAIL: TestFails") {
		t.Fatalf("second run: status %d; want TestFails to fail again\n%s", second.status, second.out)
	}
	// The failing bucket's pass may be cached from an earlier run of this
	// test; the rest are cached from this one.
	t.Setenv("FIXTURE_FAIL", "")
	third := runFixture(t, goCommand, 4)
	for n, out := range third.buckets {
		if third.status != 0 || n != failing && !cachedLine.MatchString(out) {
			t.Fatalf("third run: status %d, bucket %d not cached; want a pass with every bucket but %d cached\n%s", third.status, n, failing, third.out)
		}
	}
	for _, w := range []int{1, 2} {
		run := runFixture(t, goCommand, w)
		for n, out := range run.buckets {
			if run.status != 0 || !cachedLine.MatchString(out) {
				t.Fatalf("at %d at a time: status %d, bucket %d not cached\n%s", w, run.status, n, run.out)
			}
		}
	}
}

// TestABucketThatBuildsItsOwnBinaryIsFailedAndTheSharedOneIsKept changes
// the fixture's package after the list step, when the first bucket starts:
// that bucket builds its own binary over its link and fails with the
// reason, and every other either ran the shared binary, which no bucket
// wrote, or also failed with the reason.
func TestABucketThatBuildsItsOwnBinaryIsFailedAndTheSharedOneIsKept(t *testing.T) {
	goCommand, dir := fixture(t)
	source := filepath.Join(dir, "split", "split.go")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxyEnv, goCommand)
	t.Setenv(proxyEditEnv, source+"="+strings.Replace(string(before), "Version = 1", "Version = 2", 1))
	out := &lockedWriter{}
	r := must(newRunner(os.Args[0], out, out))
	t.Cleanup(r.close)
	p := fixturePlan(t, os.Args[0])
	binary := p.split[0].binary
	status := r.run(p, 2)
	shared, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if status != 1 || shared.Mode().Perm() != 0o555 {
		t.Fatalf("status %d, shared binary %v; want 1 and the read-only binary in place\n%s", status, shared.Mode(), out.String())
	}
	// The gate recorded the binary the list step left, before the change.
	sum := sha256.Sum256(must(os.ReadFile(binary)))
	if recorded := readFile(t, source+".sum"); recorded != hex.EncodeToString(sum[:]) {
		t.Fatalf("the shared binary is %x at the end, %s when the first bucket started; want it unchanged", sum, recorded)
	}
	reason := regexp.MustCompile(`(?m)^ao-gotest: example.com/fixture/split bucket (\d) of 8 built its own test binary: example.com/fixture/split or a package it imports changed after this run listed its tests`)
	stale := map[int]bool{}
	for _, m := range reason.FindAllStringSubmatch(out.String(), -1) {
		n, _ := strconv.Atoi(m[1])
		stale[n] = true
	}
	if len(stale) == 0 {
		t.Fatalf("no bucket named as having built its own binary\n%s", out.String())
	}
	links, err := filepath.Glob(strings.TrimSuffix(binary, ".test") + "-*.test")
	if err != nil || len(links) < 4 {
		t.Fatalf("links %v, %v", links, err)
	}
	for _, link := range links {
		n, _ := strconv.Atoi(strings.TrimSuffix(link[strings.LastIndexByte(link, '-')+1:], ".test"))
		info, err := os.Stat(link)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(shared, info) == stale[n] {
			t.Errorf("bucket %d: runs the shared binary %v, named as stale %v; want exactly one", n, os.SameFile(shared, info), stale[n])
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// lockedWriter lets the test read what the runner wrote while it runs.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
