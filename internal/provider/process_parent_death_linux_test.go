//go:build linux

package provider

import (
	"context"
	"syscall"
	"testing"

	"agent-overflow/internal/procutil"
)

// A spawn the orphan reaper does not watch gets SIGTERM when the backend
// dies. A watched one does not: the signal would end the leader before the
// reaper's SIGKILL, and a leader that is gone is one the next launch's sweep
// can no longer identify its group by.
func TestOnlyAnUnwatchedSpawnGetsTheParentDeathSignal(t *testing.T) {
	for _, c := range []struct {
		reaper procutil.Reaper
		want   syscall.Signal
	}{
		{nil, syscall.SIGTERM},
		{&recordingReaper{}, 0},
	} {
		p, err := Spawn(context.Background(), SpawnConfig{Binary: "cat", Reaper: c.reaper})
		if err != nil {
			t.Fatalf("spawn cat: %v", err)
		}
		got := p.cmd.SysProcAttr.Pdeathsig
		_ = p.Kill()
		if got != c.want {
			t.Errorf("reaped=%v: Pdeathsig = %v, want %v", c.reaper != nil, got, c.want)
		}
	}
}
