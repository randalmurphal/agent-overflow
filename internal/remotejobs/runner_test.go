package remotejobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
	"github.com/google/uuid"
)

// The test binary is the only executable these tests run. AO_TEST_REMOTE_HELPER
// selects a role; storetest.Run never sees a helper invocation.
func TestMain(m *testing.M) {
	if role := os.Getenv("AO_TEST_REMOTE_HELPER"); role != "" {
		os.Exit(remoteHelper(role))
	}
	os.Exit(providerpath.Guard(func() int { return storetest.Run(m) }))
}

func remoteHelper(role string) int {
	switch role {
	case "leave-child":
		// A command that detaches a worker and exits at once: the shape of
		// `sh -c 'server &'`. The worker keeps writing so the pipe stays open.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "AO_TEST_REMOTE_HELPER=worker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		// Give the worker a moment to write before the leader leaves, so the
		// test can prove the pipe stayed AO's after the leader exited.
		time.Sleep(300 * time.Millisecond)
		fmt.Println("leader-done")
		return 0
	case "worker":
		fmt.Println("worker-started")
		for range 600 {
			time.Sleep(50 * time.Millisecond)
			fmt.Println("worker-alive")
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown helper role")
	return 2
}

func helperArgv(t *testing.T, role string) ([]string, func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the destination runner never executes on Windows")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := func() []string {
		return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "AO_TEST_REMOTE_HELPER=" + role}
	}
	return []string{binary}, env
}

// The manager reports the sweep on the receipt without changing the result,
// and the script cleanup still happens after the leftover stop.
func TestManagerRecordsLeftoverWarningOnSuccessfulReceipt(t *testing.T) {
	t.Parallel()
	argv, env := helperArgv(t, "leave-child")
	m, _ := logManager(t, logOptions(t, 1<<20), ProcessRunner(env))
	r := request()
	r.Argv = argv
	r.TimeoutSeconds = 0
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	got := settled(t, m, r.ID)
	if got.State != "succeeded" || got.ExitCode != 0 || !strings.Contains(got.Warning, "background processes") || !strings.Contains(got.Output, "worker-started") {
		t.Fatalf("receipt: %+v", got)
	}
	entries, err := os.ReadDir(m.logs.options.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "script-") {
			t.Fatalf("script retained: %s", filepath.Join(m.logs.options.LogDir, entry.Name()))
		}
	}
}
