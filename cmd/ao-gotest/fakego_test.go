package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	// fakeOrphanEnv makes every run but the list step orphan a process
	// that exits soon after, and fail unless it is gone within 10 s.
	fakeOrphanEnv = "AO_GOTEST_FAKE_ORPHAN"
	// standInsEnv names the directory that ties a test's stand-ins to it
	// (tieStandIns).
	standInsEnv = "AO_GOTEST_STAND_INS"
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
	if ended := standIn(); ended {
		return 9
	}
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
	if os.Getenv(fakeOrphanEnv) != "" {
		if err := orphanAndWait(); err != nil {
			fmt.Fprintln(os.Stderr, err)
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
		sig, ok := nextSignal(signals)
		if !ok {
			return 9
		}
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
		// dir on its command line names the test, as a script a test
		// writes to its temp directory does.
		child := exec.Command(os.Args[0], dir)
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

// orphanAndWait starts a shell that starts a process and exits, so the
// process comes to the nearest subreaper, and waits for the process to be
// gone once it exits, as a test that ends a process group checks.
func orphanAndWait() error {
	out, err := exec.Command("sh", "-c", "sleep 0.05 & echo $!").Output()
	if err != nil {
		return fmt.Errorf("orphan a process: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("orphan a process: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			return fmt.Errorf("the orphaned process %d is still there 10 s after it was to exit: %s", pid, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// fakeChild records that it started in dir, and when SIGTERM comes, unless
// childIgnoreEnv is set, takes a while to clean up, records that it
// exited, and exits.
func fakeChild(dir string) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	if ended := standIn(); ended {
		return 9
	}
	marker := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(marker+".started", nil, 0o600); err != nil {
		return 9
	}
	for {
		if _, ok := nextSignal(signals); !ok {
			return 9
		}
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
