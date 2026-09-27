//go:build linux

package harnessclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// A contained launch on a host without a delegated cgroup starts the backend
// behind the RLIMIT_DATA fallback's shell launcher, which then execs the
// backend. Every teardown must still reach the backend it started.
func TestContainedLaunchTearsDownTheBackend(t *testing.T) {
	contained := func(t *testing.T, mode string) LaunchOptions {
		opts := fakeBackendOpts(t, mode, t.TempDir())
		opts.MemoryLimitBytes = 1 << 30
		return opts
	}

	t.Run("startup error", func(t *testing.T) {
		launched, err := Launch(context.Background(), contained(t, "startup-error"))
		if err == nil || !strings.Contains(err.Error(), "disk is full") {
			t.Fatalf("Launch error = %v, want the reported startup error", err)
		}
		assertLaunchTornDown(t, launched, err)
	})

	t.Run("bootstrap timeout", func(t *testing.T) {
		// child-only never prints its bootstrap line.
		opts := contained(t, "child-only")
		opts.Timeout = time.Second
		launched, err := Launch(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "within the deadline") {
			t.Fatalf("Launch error = %v, want the bootstrap deadline", err)
		}
		assertLaunchTornDown(t, launched, err)
	})

	for _, teardown := range []string{"terminate", "kill"} {
		t.Run(teardown+" after launch", func(t *testing.T) {
			launched, err := Launch(context.Background(), contained(t, "linger"))
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			stop := launched.Terminate
			if teardown == "kill" {
				stop = launched.Kill
			}
			if err := stop(context.Background()); err != nil {
				_ = launched.cmd.Process.Kill()
				t.Fatalf("%s: %v", teardown, err)
			}
			if instanceinfo.ProcessAlive(launched.PID) {
				_ = launched.cmd.Process.Kill()
				t.Fatalf("backend %d survived %s", launched.PID, teardown)
			}
		})
	}
}
