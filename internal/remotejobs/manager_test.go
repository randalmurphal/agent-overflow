package remotejobs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"github.com/google/uuid"
)

func request() Request {
	return Request{ID: uuid.NewString(), SourceThreadID: uuid.NewString(), Argv: []string{"injected-test-command"}, TimeoutSeconds: 60}
}
func manager(t *testing.T, run Run) (*Manager, *store.Store) {
	t.Helper()
	st := storetest.Clone(t)
	m, err := New(context.Background(), st, run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, st
}
func settled(t *testing.T, m *Manager, id string) store.RemoteJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := m.Get("owner", id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != "running" && !m.HasActive() {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("command did not settle")
	return store.RemoteJob{}
}

func TestAcceptedCommandSurvivesCallerLossAndDuplicateRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var executions atomic.Int32
	m, _ := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		executions.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return Outcome{ExitCode: -1}, ctx.Err()
		case <-release:
		}
		_, _ = io.WriteString(out, "finished after frontend disconnected")
		return Outcome{ExitCode: 0}, nil
	})
	r := request()
	project := uuid.NewString()
	if _, err := m.Start("owner", project, t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	<-started
	first, err := m.Get("owner", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if retry, err := m.Start("owner", project, first.Workspace, r); err != nil || retry.ID != r.ID {
			t.Fatalf("retry: %#v %v", retry, err)
		}
	}
	changed := r
	changed.Argv = []string{"different"}
	if _, err := m.Start("owner", project, first.Workspace, changed); err == nil {
		t.Fatal("reused ID accepted different argv")
	}
	if _, err := m.Get("other-owner", r.ID); err == nil {
		t.Fatal("other device read output")
	}
	if _, err := m.Cancel("other-owner", r.ID); err == nil {
		t.Fatal("other device canceled command")
	}
	close(release)
	result := settled(t, m, r.ID)
	if result.State != "succeeded" || result.Output != "finished after frontend disconnected" || executions.Load() != 1 {
		t.Fatalf("result=%#v executions=%d", result, executions.Load())
	}
}

// A cancellation that lands after the process already exited cleanly changed
// nothing: the receipt keeps the success the process reported.
func TestCancelAfterCleanExitReportsSuccess(t *testing.T) {
	m, _ := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		<-ctx.Done()
		_, _ = io.WriteString(out, "done")
		return Outcome{ExitCode: 0}, nil
	})
	r := request()
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel("owner", r.ID); err != nil {
		t.Fatal(err)
	}
	if got := settled(t, m, r.ID); got.State != "succeeded" || got.ExitCode != 0 || got.Error != "" || got.Output != "done" {
		t.Fatalf("late cancel overrode a clean exit: %#v", got)
	}
}

func TestCancellationAndShutdownKeepReceipts(t *testing.T) {
	m, st := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, "partial")
		<-ctx.Done()
		return Outcome{ExitCode: -1}, ctx.Err()
	})
	r := request()
	project, dir := uuid.NewString(), t.TempDir()
	if _, err := m.Start("owner", project, dir, r); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel("owner", r.ID); err != nil {
		t.Fatal(err)
	}
	if got := settled(t, m, r.ID); got.State != "canceled" {
		t.Fatalf("cancel: %#v", got)
	}
	r2 := request()
	if _, err := m.Start("owner", project, dir, r2); err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := New(context.Background(), st, func(context.Context, string, []string, io.Writer) (Outcome, error) {
		t.Error("retry spawned after restart")
		return Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	got, err := restarted.Start("owner", project, dir, r2)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("restart: %#v %v", got, err)
	}
}

func TestBoundedSlotsAndOutput(t *testing.T) {
	ready := make(chan struct{}, MaxActive)
	m, _ := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, strings.Repeat("x", store.RemoteJobOutputLimit*3)+"tail")
		ready <- struct{}{}
		<-ctx.Done()
		return Outcome{ExitCode: -1}, ctx.Err()
	})
	project, dir := uuid.NewString(), t.TempDir()
	var requests []Request
	for range MaxActive {
		r := request()
		requests = append(requests, r)
		if _, err := m.Start("owner", project, dir, r); err != nil {
			t.Fatal(err)
		}
	}
	for range MaxActive {
		<-ready
	}
	if _, err := m.Start("owner", project, dir, request()); err == nil {
		t.Fatal("unbounded execution slots")
	}
	got, err := m.Start("owner", project, dir, requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Output) != store.RemoteJobOutputLimit || !strings.HasSuffix(got.Output, "tail") || got.Truncated {
		t.Fatalf("tail: %d %v", len(got.Output), got.Truncated)
	}
}

func TestCrashAfterAcceptanceNeverExecutesAgain(t *testing.T) {
	st := storetest.Clone(t)
	r := store.RemoteJob{ID: uuid.NewString(), OwnerID: "owner", SourceThreadID: uuid.NewString(), ProjectID: uuid.NewString(), Workspace: t.TempDir(), Fingerprint: strings.Repeat("a", 64)}
	if _, fresh, err := st.AcceptRemoteJob(r); err != nil || !fresh {
		t.Fatalf("accept: %v %v", fresh, err)
	}
	m, err := New(context.Background(), st, func(context.Context, string, []string, io.Writer) (Outcome, error) {
		t.Error("recovered command executed")
		return Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.Get("owner", r.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("recovery: %#v %v", got, err)
	}
}

func TestInvalidRequestsNeverExecute(t *testing.T) {
	m, _ := manager(t, func(context.Context, string, []string, io.Writer) (Outcome, error) {
		t.Error("invalid request executed")
		return Outcome{ExitCode: 0}, nil
	})
	for _, mutate := range []func(*Request){
		func(r *Request) { r.ID = "../bad" }, func(r *Request) { r.SourceThreadID = "" }, func(r *Request) { r.Argv = nil },
		func(r *Request) { r.Argv = []string{"cmd", "a\x00b"} }, func(r *Request) { r.Argv = []string{strings.Repeat("x", 64<<10+1)} },
		func(r *Request) { r.TimeoutSeconds = -1 }, func(r *Request) { r.TimeoutSeconds = MaxTimeoutSeconds + 1 },
	} {
		r := request()
		mutate(&r)
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestCommandFailureMessagesDescribeRecoveryWithoutPrivateCauses(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
		want string
	}{
		{"missing executable", -1, &exec.Error{Name: "private-name", Err: exec.ErrNotFound}, "destination's PATH"},
		{"permissions", -1, &os.PathError{Op: "exec", Path: "/private/path", Err: os.ErrPermission}, "permissions"},
		{"exit", 7, errors.New("private process detail"), "code 7"},
		{"start failure", -1, errors.New("private process detail"), "workspace availability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := manager(t, func(context.Context, string, []string, io.Writer) (Outcome, error) {
				return Outcome{ExitCode: tc.code}, tc.err
			})
			r := request()
			if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
				t.Fatal(err)
			}
			result := settled(t, m, r.ID)
			if result.State != "failed" || !strings.Contains(result.Error, tc.want) || strings.Contains(result.Error, "private") {
				t.Fatalf("%+v", result)
			}
		})
	}
}

// The calling computer's polls are the lease. A job it stops asking about is
// stopped; a job it keeps asking about runs on past the grace.
func TestAbandonedJobsStopAfterOwnerGraceWhilePolledJobsContinue(t *testing.T) {
	o := logOptions(t, 1024)
	o.OwnerGrace = 300 * time.Millisecond
	m, _ := logManager(t, o, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		<-ctx.Done()
		return Outcome{ExitCode: -1}, ctx.Err()
	})
	project, dir := uuid.NewString(), t.TempDir()
	abandoned, polled := request(), request()
	abandoned.TimeoutSeconds, polled.TimeoutSeconds = 0, 0
	for _, r := range []Request{abandoned, polled} {
		if _, err := m.Start("owner", project, dir, r); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * o.OwnerGrace)
	for time.Now().Before(deadline) {
		if _, err := m.Get("owner", polled.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Get("other-owner", abandoned.ID); err == nil {
			t.Fatal("another device's read counted as owner contact")
		}
		time.Sleep(o.OwnerGrace / 4)
	}
	got, err := m.Get("owner", abandoned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "canceled" || !strings.Contains(got.Error, "stopped checking") {
		t.Fatalf("abandoned job: %+v", got)
	}
	if still, err := m.Get("owner", polled.ID); err != nil || still.State != "running" {
		t.Fatalf("polled job: %+v %v", still, err)
	}
	if _, err := m.Cancel("owner", polled.ID); err != nil {
		t.Fatal(err)
	}
	if got := settled(t, m, polled.ID); got.State != "canceled" || strings.Contains(got.Error, "stopped checking") {
		t.Fatalf("explicit cancel: %+v", got)
	}
}
