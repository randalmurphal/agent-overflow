//go:build !windows

package provider

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// recordingReaper records the groups a spawn has watched and released.
type recordingReaper struct {
	mu       sync.Mutex
	watched  []int
	released []int
}

func (r *recordingReaper) Watch(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watched = append(r.watched, pgid)
}

func (r *recordingReaper) Release(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, pgid)
}

func (r *recordingReaper) calls() (watched, released []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.watched), slices.Clone(r.released)
}

// A reaped spawn's group is watched before Spawn returns, so a backend that
// dies at any point after the fork leaves the reaper holding it, and released
// once by the time the process is known to be gone, whether it was closed or
// exited on its own.
func TestAReapedSpawnIsWatchedFromItsStartUntilItExits(t *testing.T) {
	for _, c := range []struct {
		name   string
		binary string
		end    func(*Process)
	}{
		{"closed", "cat", func(p *Process) { _ = p.Close() }},
		{"exited on its own", "true", func(p *Process) { <-p.Done() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			reaper := &recordingReaper{}
			p, err := Spawn(context.Background(), SpawnConfig{Binary: c.binary, Reaper: reaper})
			if err != nil {
				t.Fatal(err)
			}
			pid := p.PID()
			if watched, _ := reaper.calls(); !slices.Equal(watched, []int{pid}) {
				t.Fatalf("watched %v when Spawn returned, want [%d]", watched, pid)
			}
			c.end(p)
			watched, released := reaper.calls()
			if !slices.Equal(watched, []int{pid}) || !slices.Equal(released, []int{pid}) {
				t.Fatalf("watched %v released %v once the process was gone, want [%d] each", watched, released, pid)
			}
		})
	}
}
