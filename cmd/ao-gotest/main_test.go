package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/testutil/testshard/shardspec"
)

// This test binary is also the go command the runs under test start, and
// the command itself for the runs that replace their process.
const (
	// fakeEnv makes the binary the go command: it logs each `go test` to
	// the file fakeEnv names and plays it as the fake* variables say, and
	// runs `go list` with the go command realGoEnv names.
	fakeEnv   = "AO_GOTEST_FAKE_LOG"
	realGoEnv = "AO_GOTEST_REAL_GO"
	// fakeFailEnv is the shard ("i/K") that exits 3, or "whole" for the
	// unsplit run.
	fakeFailEnv = "AO_GOTEST_FAKE_FAIL"
	// fakeBuildEnv is the exit status of `go test -c`.
	fakeBuildEnv = "AO_GOTEST_FAKE_BUILD"
	// fakeHoldEnv makes every run but the build wait for a signal.
	fakeHoldEnv = "AO_GOTEST_FAKE_HOLD"
	// mainEnv makes the binary this command.
	mainEnv = "AO_GOTEST_MAIN"
	// markEnv is a variable the fake logs, to show what environment it got.
	markEnv = "AO_GOTEST_MARK"
)

const fakeLines = 40

func TestMain(m *testing.M) {
	// The go command's first argument is its subcommand; this command's is
	// the go command.
	if log := os.Getenv(fakeEnv); log != "" && len(os.Args) > 1 && (os.Args[1] == "test" || os.Args[1] == "list") {
		os.Exit(fakeGo(log, os.Args[1:]))
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

func fakeGo(log string, args []string) int {
	if args[0] == "list" {
		real := os.Getenv(realGoEnv)
		err := syscall.Exec(real, append([]string{"go"}, args...), os.Environ())
		fmt.Fprintln(os.Stderr, "exec", real, err)
		return 9
	}
	shard := os.Getenv(shardspec.Env)
	if err := appendLog(log, shard+"|go "+strings.Join(args, " ")+"|"+os.Getenv(markEnv)); err != nil {
		return 9
	}
	if slices.Contains(args, "-c") {
		status, _ := strconv.Atoi(os.Getenv(fakeBuildEnv))
		return status
	}
	if os.Getenv(fakeHoldEnv) != "" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := appendLog(log, "held "+shard); err != nil {
			return 9
		}
		sig := <-signals
		fmt.Println("got", sig, shard)
		return 128 + int(sig.(syscall.Signal))
	}
	name := "whole"
	if shard != "" {
		name = "shard " + shard
	}
	for i := range fakeLines {
		// Half a line at a time, so a reader that passed writes through
		// as they came would split lines of different processes.
		fmt.Fprintf(os.Stdout, "%s line", name)
		time.Sleep(200 * time.Microsecond)
		fmt.Fprintf(os.Stdout, " %d\n", i)
		fmt.Fprintf(os.Stderr, "%s err %d\n", name, i)
	}
	if fail := os.Getenv(fakeFailEnv); fail != "" && (fail == shard || (fail == "whole" && shard == "")) {
		return 3
	}
	return 0
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

func testPlan(t *testing.T, k int) runPlan {
	t.Helper()
	inv, err := parseArgs([]string{"go", "test", "-timeout", "30m", "-count=1", "example/whole", "example/split"})
	if err != nil {
		t.Fatal(err)
	}
	return plan(inv, []string{"example/split"}, []string{"example/whole"}, k)
}

// TestASplitRunPrintsWholeLinesAndEachProcessWhole runs a split package's
// processes and an unsplit run at once and checks the calls each made, that
// no line of one is cut by another's, and that each process's output is
// one block under its header.
func TestASplitRunPrintsWholeLinesAndEachProcessWhole(t *testing.T) {
	r, stdout, stderr, log := fakeRunner(t, fakeFailEnv+"=1/3")
	status := r.run(testPlan(t, 3))
	if status != 3 {
		t.Errorf("status %d; want 3, the failed shard's", status)
	}
	got := calls(t, log)
	want := []string{
		"0/3|go test -timeout 30m -count=1 -parallel 1 example/split|",
		"1/3|go test -timeout 30m -count=1 -parallel 1 example/split|",
		"2/3|go test -timeout 30m -count=1 -parallel 1 example/split|",
		"|go test -c -o " + os.DevNull + " example/split|",
		"|go test -timeout 30m -count=1 example/whole|",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkBlocks(t, "stdout", stdout.String(), "line", true)
	checkBlocks(t, "stderr", stderr.String(), "err", false)
}

// checkBlocks checks that every line is one the fake wrote whole, that the
// unsplit run's lines came in order, and that each shard's lines came as
// one block of consecutive lines, after its header when headed.
func checkBlocks(t *testing.T, stream, out, word string, headed bool) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	whole := 0
	shards := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		shard, isHeader := strings.CutPrefix(lines[i], "ao-gotest: example/split shard ")
		if isHeader != headed && (isHeader || strings.HasPrefix(lines[i], "shard ")) {
			t.Fatalf("%s: line %q; want a header before each shard's block only in stdout", stream, lines[i])
		}
		if !isHeader {
			shard = ""
			if owner, _, ok := strings.Cut(lines[i], " "+word+" "); ok && strings.HasPrefix(owner, "shard ") {
				shard = strings.TrimPrefix(owner, "shard ")
				i--
			}
		}
		if shard != "" {
			for n := range fakeLines {
				i++
				want := fmt.Sprintf("shard %s %s %d", shard, word, n)
				if i >= len(lines) || lines[i] != want {
					t.Fatalf("%s: in shard %s's block, line %d is %q; want %q", stream, shard, n, lines[min(i, len(lines)-1)], want)
				}
			}
			shards[shard] = true
			continue
		}
		if want := fmt.Sprintf("whole %s %d", word, whole); lines[i] != want {
			t.Fatalf("%s: line %q; want %q, the unsplit run's next", stream, lines[i], want)
		}
		whole++
	}
	if whole != fakeLines || len(shards) != 3 {
		t.Errorf("%s: %d lines of the unsplit run and the blocks of shards %v; want %d and three", stream, whole, shards, fakeLines)
	}
}

// TestAFailedBuildRunsThePackageOnceUnsplit checks that a split package
// whose test build fails runs as one `go test`, which reports the failure
// once, and no shard starts.
func TestAFailedBuildRunsThePackageOnceUnsplit(t *testing.T) {
	r, _, _, log := fakeRunner(t, fakeBuildEnv+"=1", fakeFailEnv+"=whole")
	if status := r.run(testPlan(t, 3)); status != 3 {
		t.Errorf("status %d; want 3, the unsplit runs'", status)
	}
	got := calls(t, log)
	want := []string{
		"|go test -c -o " + os.DevNull + " example/split|",
		"|go test -timeout 30m -count=1 example/split|",
		"|go test -timeout 30m -count=1 example/whole|",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestASignalReachesEveryRunningProcessAndStartsNoMore holds every run
// open, signals the runner and checks that each running `go test` got the
// signal and ended with it, and that no queued shard started after it.
func TestASignalReachesEveryRunningProcessAndStartsNoMore(t *testing.T) {
	r, stdout, _, log := fakeRunner(t, fakeHoldEnv+"=1")
	done := make(chan int)
	// Two slots for four shards: two shards are queued when the signal
	// comes.
	p := testPlan(t, 4)
	p.k = 2
	go func() { done <- r.run(p) }()
	// The unsplit run and the two shards in the slots hold, each once its
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
		t.Errorf("calls:\n%s\nwant the build, the unsplit run and two shards", strings.Join(got, "\n"))
	}
}

// TestARunThatSplitsNothingIsTheOneGoTest runs the command for runs it must
// not split and checks that each replaced itself with the `go test` command
// line it was given, in an environment it did not change.
func TestARunThatSplitsNothingIsTheOneGoTest(t *testing.T) {
	reporoot.ObserveGoSources(t)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	// The command finds this binary as its go command.
	bin := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	env := append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, shardspec.Env+"=") || strings.HasPrefix(kv, "PATH=")
	}), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), realGoEnv+"="+realGo, mainEnv+"=1", markEnv+"=kept")
	// internal/store's TestMain calls Select; testshard's own tests do not
	// import it.
	const split, whole = "agent-overflow/internal/store", shardspec.SelectPackage
	for _, tc := range []struct {
		name  string
		env   string
		args  []string
		split bool
	}{
		{name: "one core", env: "GOMAXPROCS=1", args: []string{"-timeout", "30m", "-count=1", split}},
		{name: "a narrowed run", env: "GOMAXPROCS=4", args: []string{"-timeout", "30m", "-run", "TestX", "-count=1", split}},
		{name: "a profile", env: "GOMAXPROCS=4", args: []string{"-cpuprofile", "cpu.out", split}},
		{name: "no package opts in", env: "GOMAXPROCS=4", args: []string{"-timeout", "30m", "-count=1", whole}},
		{name: "a package opts in", env: "GOMAXPROCS=4", args: []string{"-timeout", "30m", "-count=1", split}, split: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			// The command line make go-test gives it.
			command := exec.Command(os.Args[0], append([]string{"go", "test"}, tc.args...)...)
			command.Dir = reporoot.Dir(t)
			command.Env = append(slices.Clone(env), tc.env, fakeEnv+"="+log)
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := calls(t, log)
			if tc.split {
				if len(got) != 5 || got[0] != "0/4|go test -timeout 30m -count=1 -parallel 1 "+split+"|kept" || !strings.HasPrefix(got[4], "|go test -c ") {
					t.Fatalf("calls %q; want four shards and the build", got)
				}
				return
			}
			// The variable the run was given reaches the one go test.
			want := "|go test " + strings.Join(tc.args, " ") + "|kept"
			if len(got) != 1 || got[0] != want {
				t.Fatalf("calls %q; want [%q]", got, want)
			}
		})
	}
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
