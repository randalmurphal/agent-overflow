package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/governor"
)

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
		"GOTMPDIR="+gotmp, "TMPDIR="+t.TempDir(), mainEnv+"=1", markEnv+"=kept", standInsEnv+"="+tieStandIns(t))
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
// not split and checks that each ran the `go test` command line it was
// given, as one `go test`, in an environment it did not change; and that
// one that splits, under make's GOFLAGS, reserves in its own governor,
// lists the package first into a directory on GOTMPDIR, and leaves no
// directory of its own or of a run that was killed.
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

// TestANarrowedRunThatLeavesAProcessFails: a run that splits nothing runs
// under a leader too, so a process its go test leaves is ended and named,
// and the run fails though the go test passed.
func TestANarrowedRunThatLeavesAProcessFails(t *testing.T) {
	kids := t.TempDir()
	command := commandEnv(t, t.TempDir(), "-run", "TestX", splitPkg)
	command.Env = append(command.Env, fakeEnv+"="+filepath.Join(t.TempDir(), "calls"), fakeChildEnv+"="+kids, childIgnoreEnv+"=1", graceEnv+"=100ms")
	out, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit %v; want status 1\n%s", err, out)
	}
	checkChildrenGone(t, kids, childrenPerRun(), false)
	for pid := range recorded(t, kids) {
		if named := fmt.Sprintf("\nao-gotest:   pid %d", pid); !strings.Contains(string(out), named) {
			t.Errorf("output:\n%s\nwant child %d named", out, pid)
		}
	}
}

// TestAnUnsplitRunStreamsItsOutputAndCtrlCEndsIt runs a narrowed run, which
// splits nothing, as a shell runs it, in a process group of its own. A line
// the go test writes arrives while it still runs, and SIGINT to the group,
// what Ctrl-C sends, ends the go test and every process it started, with
// the status a shell gives a process that SIGINT ended.
func TestAnUnsplitRunStreamsItsOutputAndCtrlCEndsIt(t *testing.T) {
	kids := t.TempDir()
	command := commandEnv(t, t.TempDir(), "-run", "TestX", "-v", splitPkg)
	command.Env = append(command.Env, fakeEnv+"="+filepath.Join(t.TempDir(), "calls"), fakeHoldEnv+"=1", fakeChildEnv+"="+kids)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &lockedWriter{}
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
	})
	lines := bufio.NewScanner(stdout)
	// The fake holds until it is signaled, so its first line can only have
	// come while it ran.
	if !lines.Scan() || lines.Text() != "before hold bucket TestX" {
		t.Fatalf("first line %q, %v; want the go test's line while it holds\nstderr:\n%s", lines.Text(), lines.Err(), stderr.String())
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var rest []string
	for lines.Scan() {
		rest = append(rest, lines.Text())
	}
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGINT) {
		t.Fatalf("exit %v; want status %d\nstdout after the first line: %q\nstderr:\n%s", err, 128+int(syscall.SIGINT), rest, stderr.String())
	}
	checkChildrenGone(t, kids, childrenPerRun(), false)
	if strings.Contains(stderr.String(), "the go test left") {
		t.Errorf("stderr:\n%s\nwant no process named as left by a run that was stopped", stderr.String())
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
			var waitErr error
			exited := make(chan struct{})
			go func() { waitErr = command.Wait(); close(exited) }()
			// However the test ends, the command is gone before its
			// stand-ins are checked.
			t.Cleanup(func() {
				_ = command.Process.Kill()
				<-exited
			})
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
			case <-exited:
				err = waitErr
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
