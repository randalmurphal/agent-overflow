package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	// fakeHoldEnv makes every run but the list step wait for a signal.
	fakeHoldEnv = "AO_GOTEST_FAKE_HOLD"
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
	if os.Getenv(mainEnv) != "" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	if os.Getenv(orphanEnv) != "" {
		if _, _, err := ownedtemp.Make("", runDirPrefix); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
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
		if err := os.WriteFile(flagValue(args, "-o"), []byte("binary"), 0o755); err != nil {
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
	if os.Getenv(fakeHoldEnv) != "" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := appendLog(log, "held "+name); err != nil {
			return 9
		}
		sig := <-signals
		fmt.Println("got", sig, name)
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
		{"-timeout 30m -count 1 -json -v ./a ./b", true, "./a ./b"},
		{"./a -count 1 ./b --json", true, "./a ./b"},
		{"--count=2 --json", true, ""},
		{"-tags noremote -count=1 ./a", true, "./a"},
		{"-tags=noremote -vet=off -short -failfast -shuffle on -skip TestX -p 4", true, ""},
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
	splits, rest, err := parseSplits([]string{"-split=example/a=512", "-split", "example/b=64", "go", "test", "-split=x=1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []split{{"example/a", 512 << 20}, {"example/b", 64 << 20}}
	if !slices.Equal(splits, want) || !slices.Equal(rest, []string{"go", "test", "-split=x=1"}) {
		t.Fatalf("splits %v, rest %q; want %v and the go command line", splits, rest, want)
	}
	for _, bad := range []string{"example/a", "example/a=", "=512", "example/a=0", "example/a=1.5", "example/a=-1"} {
		if _, _, err := parseSplits([]string{"-split=" + bad, "go", "test"}); err == nil {
			t.Errorf("-split=%s parsed; want an error", bad)
		}
	}
}

// TestReserveWorkersRunsWhatTheGovernorGrantsAndAtLeastOne reserves
// against a governor whose host has 2.5 units above its floor, then none:
// two buckets at a time under one lease the release removes, then one at a
// time with no lease.
func TestReserveWorkersRunsWhatTheGovernorGrantsAndAtLeastOne(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	memory := &fixedMemory{available: floor + 5*unit/2}
	m, err := governor.New(governor.Options{Dir: t.TempDir(), AvailableFloorBytes: floor, Memory: memory})
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner("go", &lockedWriter{}, &lockedWriter{})
	req := governor.Request{RunID: "go-test", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()}
	leases := func() int {
		t.Helper()
		snap, err := m.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return len(snap.Leases)
	}
	units := []uint64{unit, unit, unit, unit}
	w, release, reserved := reserveWorkers(r, m, req, units)
	if w != 2 || reserved != "200 MiB reserved" || leases() != 1 {
		t.Fatalf("%d at a time (%s), %d leases; want 2 under one lease", w, reserved, leases())
	}
	if err := release(); err != nil || leases() != 0 {
		t.Fatalf("release: %v, %d leases left; want none", err, leases())
	}
	memory.available = floor - 1
	w, release, reserved = reserveWorkers(r, m, req, units)
	if w != 1 || !strings.HasPrefix(reserved, "no 100 MiB free") || leases() != 0 || release() != nil {
		t.Fatalf("%d at a time (%s), %d leases; want 1 with no lease", w, reserved, leases())
	}
	if len(r.notes) != 0 {
		t.Fatalf("notes %q; want none for a grant of nothing", r.notes)
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
	return newRunner(os.Args[0], stdout, stderr), stdout, stderr, log
}

// calls are the fake's logged calls, sorted, without the hold lines.
func calls(t *testing.T, log string) []string {
	t.Helper()
	var got []string
	for line := range strings.Lines(readFile(t, log)) {
		if !strings.HasPrefix(line, "held ") {
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
	return plan(inv, []split{{"example/split", unit}}, []string{"example/whole"}, dir)
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
		last := lines[len(lines)-1]
		if named := strings.HasPrefix(last, "ao-gotest: example/split: a bucket's largest process reached ") && strings.HasSuffix(last, fmt.Sprintf(" MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", tc.unit>>20)); named != tc.named {
			t.Errorf("unit %d MiB: last line %q; named %v, want %v", tc.unit>>20, last, named, tc.named)
		}
	}
}

// TestASignalReachesEveryRunningProcessAndStartsNoMore holds every run
// open, signals the runner and checks that each running `go test` got the
// signal and ended with it, and that no queued bucket started after it.
func TestASignalReachesEveryRunningProcessAndStartsNoMore(t *testing.T) {
	r, stdout, _, log := fakeRunner(t, fakeHoldEnv+"=1")
	done := make(chan int)
	// Two slots for more buckets: the rest are queued when the signal
	// comes.
	go func() { done <- r.run(testPlan(t, t.TempDir(), 1<<40), 2) }()
	// The unsplit run and the buckets in the two slots hold, each once its
	// signal handler is set.
	deadline := time.Now().Add(time.Minute)
	for strings.Count(readFile(t, log), "held ") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the runs never held:\n%s", readFile(t, log))
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.signal(syscall.SIGTERM)
	var status int
	select {
	case status = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the runner did not end after the signal")
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

// TestARunThatSplitsNothingIsTheOneGoTest runs the command for runs it must
// not split and checks that each replaced itself with the `go test` command
// line it was given, in an environment it did not change; and that one
// that splits reserves in its own governor, lists the package first, and
// leaves no directory of its own or of a run that was killed.
func TestARunThatSplitsNothingIsTheOneGoTest(t *testing.T) {
	// The command finds this binary as its go command.
	bin := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	// The governor's directory is under the cache directory of HOME.
	home := t.TempDir()
	env := append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CACHE_HOME=")
	}), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home, mainEnv+"=1", markEnv+"=kept")
	const split, whole = "example/split", "example/whole"
	for _, tc := range []struct {
		name  string
		args  []string
		split bool
	}{
		{name: "a narrowed run", args: []string{"-timeout", "30m", "-run", "TestX", "-count=1", split}},
		{name: "a profile", args: []string{"-cpuprofile", "cpu.out", split}},
		{name: "no package is declared", args: []string{"-timeout", "30m", "-count=1", whole}},
		{name: "a package is declared", args: []string{"-timeout", "30m", "-count=1", split}, split: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			tmp := t.TempDir()
			orphan := exec.Command(os.Args[0])
			orphan.Env = append(os.Environ(), orphanEnv+"=1", "TMPDIR="+tmp)
			if out, err := orphan.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			// The command line make go-test gives it.
			command := exec.Command(os.Args[0], append([]string{"-split=" + split + "=1", "go", "test"}, tc.args...)...)
			command.Dir = t.TempDir()
			command.Env = append(slices.Clone(env), fakeEnv+"="+log, "TMPDIR="+tmp)
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := calls(t, log)
			if tc.split {
				if len(got) != 1+len(wantCalls(""))-2 || !slices.ContainsFunc(got, func(call string) bool {
					return strings.Contains(call, " -list . "+split+"|kept")
				}) || !strings.Contains(string(out), "ao-gotest: "+split+": 8 buckets each, ") {
					t.Fatalf("calls %q\n%s\nwant the list step and the buckets", got, out)
				}
				if left, err := filepath.Glob(filepath.Join(tmp, runDirPrefix+"*")); err != nil || len(left) != 0 {
					t.Fatalf("left %v, %v; want the run's directory and the killed run's removed", left, err)
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
	return plan(inv, []split{{"example.com/fixture/split", 1 << 40}}, nil, t.TempDir())
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
	r := newRunner(goCommand, out, out)
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
	r := newRunner(os.Args[0], out, out)
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
