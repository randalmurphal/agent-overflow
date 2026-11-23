//go:build linux

package providerpath

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// markEnvPrefix starts the name of the variable Guard sets in this
// process's environment before the tests run. Every process a test starts
// inherits it however it is reparented or grouped, unless the code that
// starts it builds a fresh environment: the orphan reaper's sidecar gets
// PATH alone. The name ends in this process's pid and random bytes, so a
// test binary that a test runs adds its own and keeps this one's. It does
// not start with AO_, which agent watch scripts are started without.
const markEnvPrefix = "AGENT_OVERFLOW_TEST_PROCESS_"

// leftoverGrace is how long a process a test started may take to exit once
// the tests are done before it counts as left running.
const leftoverGrace = 5 * time.Second

// markProcesses sets the variable and returns the check Guard runs after
// the tests: every process still alive with the variable once
// leftoverGrace has passed is named and killed, and the check fails.
func markProcesses() (check func() error, err error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("mark this test process's children: %w", err)
	}
	name := fmt.Sprintf("%s%d_%x", markEnvPrefix, os.Getpid(), id)
	if err := os.Setenv(name, "1"); err != nil {
		return nil, fmt.Errorf("mark this test process's children: %w", err)
	}
	entry := []byte(name + "=1")
	return func() error { return endLeftovers(entry) }, nil
}

func endLeftovers(entry []byte) error {
	deadline := time.Now().Add(leftoverGrace)
	for {
		pids, err := marked(entry)
		if err != nil || len(pids) == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return killLeftovers(pids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// marked lists the processes other than this one whose environment holds
// entry. A process that exits meanwhile, or that another user owns, is not
// one; a zombie's environment reads empty.
func marked(entry []byte) ([]int, error) {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	self := os.Getpid()
	var pids []int
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == self {
			continue
		}
		environ, err := os.ReadFile("/proc/" + proc.Name() + "/environ")
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) || errors.Is(err, fs.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the environment of pid %d: %w", pid, err)
		}
		for variable := range bytes.SplitSeq(environ, []byte{0}) {
			if bytes.Equal(variable, entry) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

func killLeftovers(pids []int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "providerpath: the tests left %d process(es) running %s after they ended, killed now; the test or fixture that started each must end it:", len(pids), leftoverGrace)
	for _, pid := range pids {
		dir := "/proc/" + strconv.Itoa(pid)
		cmdline, _ := os.ReadFile(dir + "/cmdline")
		cwd, _ := os.Readlink(dir + "/cwd")
		fmt.Fprintf(&b, "\n  pid %d in %s: %s", pid, cwd, strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}))))
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			fmt.Fprintf(&b, " (kill: %v)", err)
		}
	}
	return errors.New(b.String())
}
