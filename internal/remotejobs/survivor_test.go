package remotejobs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/store"
)

// survivingRun is a runner whose stop could not end the command: it waits
// for its context, then reports a group that outlived SIGKILL.
func survivingRun(ctx context.Context, _ string, _ []string, _ io.Writer) (Outcome, error) {
	<-ctx.Done()
	return Outcome{ExitCode: -1, Leftovers: true}, procutil.ErrGroupSurvived
}

// requireSurvivorReported fails unless the receipt keeps the ending's state,
// says why it ended and that the process is still running, and neither
// claims the leftovers were stopped nor offers a fresh start.
func requireSurvivorReported(t *testing.T, got store.RemoteJob, state, why string) {
	t.Helper()
	if got.State != state || !strings.Contains(got.Error, why) || !strings.Contains(got.Error, "still running on the destination") {
		t.Fatalf("receipt %+v, want state %q saying %q and that the process still runs", got, state, why)
	}
	if strings.Contains(got.Error, "Start it again") {
		t.Fatalf("receipt offers a fresh start while the process may run: %q", got.Error)
	}
	if strings.Contains(got.Warning, "were stopped") || !strings.Contains(got.Warning, "could not all be stopped") {
		t.Fatalf("warning %q, want leftovers reported as not stopped", got.Warning)
	}
}

// settledUnpolled waits for the job to finish without reading it as its
// owner, which would renew the lease.
func settledUnpolled(t *testing.T, m *Manager, id string) store.RemoteJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for m.HasActive() {
		if time.Now().After(deadline) {
			t.Fatal("command did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	got, err := m.Get("owner", id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A group that outlived its stop is reported as still running whichever way
// the run ended.
func TestASurvivingGroupIsReportedOnEveryEnding(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		m, _ := manager(t, survivingRun)
		r := request()
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Cancel("owner", r.ID); err != nil {
			t.Fatal(err)
		}
		requireSurvivorReported(t, settled(t, m, r.ID), "canceled", "was canceled")
	})
	t.Run("deadline", func(t *testing.T) {
		m, _ := manager(t, survivingRun)
		r := request()
		r.TimeoutSeconds = 1
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
			t.Fatal(err)
		}
		requireSurvivorReported(t, settled(t, m, r.ID), "failed", "time limit")
	})
	t.Run("abandoned lease", func(t *testing.T) {
		o := logOptions(t, 1024)
		o.OwnerGrace = 200 * time.Millisecond
		m, _ := logManager(t, o, survivingRun)
		r := request()
		r.TimeoutSeconds = 0
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
			t.Fatal(err)
		}
		requireSurvivorReported(t, settledUnpolled(t, m, r.ID), "canceled", "stopped checking")
	})
	t.Run("shutdown", func(t *testing.T) {
		m, st := manager(t, survivingRun)
		r := request()
		project, dir := uuid.NewString(), t.TempDir()
		if _, err := m.Start("owner", project, dir, r); err != nil {
			t.Fatal(err)
		}
		m.Close()
		restarted, err := New(context.Background(), st, survivingRun)
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close()
		got, err := restarted.Get("owner", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		requireSurvivorReported(t, got, "interrupted", "computer stopped")
	})
	t.Run("clean exit with a failed sweep", func(t *testing.T) {
		m, _ := manager(t, func(context.Context, string, []string, io.Writer) (Outcome, error) {
			return Outcome{ExitCode: 0, Leftovers: true}, procutil.ErrGroupSurvived
		})
		r := request()
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
			t.Fatal(err)
		}
		requireSurvivorReported(t, settled(t, m, r.ID), "failed", "could not be stopped")
	})
}

// Leftovers the sweep ended are reported as stopped.
func TestSweptLeftoversAreReportedStopped(t *testing.T) {
	m, _ := manager(t, func(context.Context, string, []string, io.Writer) (Outcome, error) {
		return Outcome{ExitCode: 0, Leftovers: true}, nil
	})
	r := request()
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	if got := settled(t, m, r.ID); got.State != "succeeded" || got.Error != "" || !strings.Contains(got.Warning, "were stopped") {
		t.Fatalf("receipt %+v, want success warning that leftovers were stopped", got)
	}
}
