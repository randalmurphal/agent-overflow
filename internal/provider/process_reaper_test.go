//go:build !windows

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
