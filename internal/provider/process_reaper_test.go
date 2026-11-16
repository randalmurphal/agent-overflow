//go:build !windows

package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/procutil/procutiltest"
)

// testLifetime is the lifetime this package's tests give a launch that is
// not about lifetimes.
var testLifetime = Watched(procutiltest.Reaper())

// A launch's group is watched before Spawn returns, so a backend that dies at
// any point after the fork leaves the reaper holding it, and released once by
// the time the process is known to be gone, whether it was closed or exited on
// its own. Both lifetimes are watched.
func TestALaunchIsWatchedFromItsStartUntilItExits(t *testing.T) {
	for _, lifetime := range []struct {
		name string
		make func(procutil.Reaper) Lifetime
	}{{"watched", Watched}, {"bounded", Bounded}} {
		for _, c := range []struct {
			name   string
			binary string
			end    func(*Process)
		}{
			{"closed", "cat", func(p *Process) { _ = p.Close() }},
			{"exited on its own", "true", func(p *Process) { <-p.Done() }},
		} {
			t.Run(lifetime.name+" "+c.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				reaper := &procutiltest.Recorder{}
				p, err := Spawn(ctx, lifetime.make(reaper), SpawnConfig{Binary: c.binary})
				if err != nil {
					t.Fatal(err)
				}
				pid := p.PID()
				if watched, _ := reaper.Calls(); !slices.Equal(watched, []int{pid}) {
					t.Fatalf("watched %v when Spawn returned, want [%d]", watched, pid)
				}
				c.end(p)
				watched, released := reaper.Calls()
				if !slices.Equal(watched, []int{pid}) || !slices.Equal(released, []int{pid}) {
					t.Fatalf("watched %v released %v once the process was gone, want [%d] each", watched, released, pid)
				}
			})
		}
	}
}

// A launch that does not say what ends it with the backend, or a bounded one
// with no deadline, is refused before anything starts.
func TestALaunchWithoutALifetimeStartsNothing(t *testing.T) {
	for _, c := range []struct {
		name     string
		lifetime Lifetime
		want     error
	}{
		{"no lifetime", Lifetime{}, ErrNoLifetime},
		{"watched by no reaper", Watched(nil), ErrNoLifetime},
		{"bounded by no reaper", Bounded(nil), ErrNoLifetime},
		{"bounded with no deadline", Bounded(&procutiltest.Recorder{}), ErrUnbounded},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			p, err := Spawn(context.Background(), c.lifetime, SpawnConfig{Binary: "sh", Args: []string{"-c", "touch " + marker}})
			if err == nil {
				_ = p.Kill()
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Spawn = %v, want %v", err, c.want)
			}
			time.Sleep(50 * time.Millisecond)
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("the refused launch ran")
			}
		})
	}
}

// spawnLeavingAChild starts a leader that starts child in the background,
// prints its pid and exits, and returns the child's pid. The child is killed
// at cleanup if the test leaves it running.
func spawnLeavingAChild(t *testing.T, lifetime Lifetime, child string) (*Process, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	p, err := Spawn(ctx, lifetime, SpawnConfig{Binary: "sh", Args: []string{"-c", child + " & echo $!; exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(string(line))
	if err != nil {
		t.Fatalf("child pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return p, pid
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still exists once the launch was done (kill 0: %v)", pid, err)
	}
}

// The leader's exit is not its group's: what it left running is ended, and
// the group is released only once it is empty, before Done closes. Both
// lifetimes.
func TestALaunchsLeftoversEndBeforeItsGroupIsReleased(t *testing.T) {
	for _, lifetime := range []struct {
		name string
		make func(procutil.Reaper) Lifetime
	}{{"watched", Watched}, {"bounded", Bounded}} {
		t.Run(lifetime.name, func(t *testing.T) {
			reaper := &procutiltest.Recorder{}
			p, child := spawnLeavingAChild(t, lifetime.make(reaper), "sleep 120")
			<-p.Done()
			requireGone(t, child)
			if _, released := reaper.Calls(); !slices.Equal(released, []int{p.PID()}) {
				t.Fatalf("released %v, want [%d]", released, p.PID())
			}
			if err := p.Close(); err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		})
	}
}

// A leftover that ignores SIGTERM is killed after killGrace, and Close
// returns only once it is gone.
func TestCloseAfterTheLeaderExitsReturnsOnceALeftoverIgnoringTermIsKilled(t *testing.T) {
	shortenGraces(t, 100*time.Millisecond, 200*time.Millisecond)
	reaper := &procutiltest.Recorder{}
	p, child := spawnLeavingAChild(t, Watched(reaper), `(trap "" TERM; exec sleep 120)`)
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	requireGone(t, child)
	if elapsed := time.Since(start); elapsed < killGrace {
		t.Fatalf("Close returned after %v, want the leftover given killGrace before SIGKILL", elapsed)
	}
	if _, released := reaper.Calls(); !slices.Equal(released, []int{p.PID()}) {
		t.Fatalf("released %v, want [%d]", released, p.PID())
	}
}

// A leftover that cannot be ended keeps its group watched, for the reaper to
// end with the backend, and Close and Kill report it.
func TestALeftoverThatCannotBeEndedKeepsItsGroupWatched(t *testing.T) {
	prev := stopLeftovers
	stopLeftovers = func(*exec.Cmd, time.Duration) error { return procutil.ErrGroupSurvived }
	t.Cleanup(func() { stopLeftovers = prev })
	reaper := &procutiltest.Recorder{}
	p, _ := spawnLeavingAChild(t, Watched(reaper), "sleep 120")
	if err := p.Close(); !errors.Is(err, procutil.ErrGroupSurvived) {
		t.Fatalf("Close = %v, want ErrGroupSurvived", err)
	}
	if err := p.Kill(); !errors.Is(err, procutil.ErrGroupSurvived) {
		t.Fatalf("Kill = %v, want ErrGroupSurvived", err)
	}
	if _, released := reaper.Calls(); len(released) != 0 {
		t.Fatalf("released %v, want the group still watched", released)
	}
}

// A graceful cancel stops the whole group, so a helper that ignores SIGTERM
// is gone once the cancelled launch's Close returns, and the group is
// released then.
func TestGracefulCancelEndsAChildThatIgnoresTerm(t *testing.T) {
	shortenGraces(t, shutdownGrace, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reaper := &procutiltest.Recorder{}
	p, err := Spawn(ctx, Watched(reaper), SpawnConfig{
		Binary: "sh", Args: []string{"-c", `sh -c 'trap "" TERM; echo $$; exec sleep 120' & exec sleep 120`},
		GracefulCancel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	child, err := strconv.Atoi(string(line))
	if err != nil {
		t.Fatalf("child pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	cancel()
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	requireGone(t, child)
	if _, released := reaper.Calls(); !slices.Equal(released, []int{p.PID()}) {
		t.Fatalf("released %v, want [%d]", released, p.PID())
	}
}
