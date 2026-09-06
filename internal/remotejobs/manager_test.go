package remotejobs

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

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
	m, _ := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (int, error) {
		executions.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-release:
		}
		_, _ = io.WriteString(out, "finished after frontend disconnected")
		return 0, nil
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

func TestCancellationAndShutdownKeepReceipts(t *testing.T) {
	m, st := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (int, error) {
		_, _ = io.WriteString(out, "partial")
		<-ctx.Done()
		return -1, ctx.Err()
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
	restarted, err := New(context.Background(), st, func(context.Context, string, []string, io.Writer) (int, error) {
		t.Error("retry spawned after restart")
		return 0, nil
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
	m, _ := manager(t, func(ctx context.Context, _ string, _ []string, out io.Writer) (int, error) {
		_, _ = io.WriteString(out, strings.Repeat("x", store.RemoteJobOutputLimit*3)+"tail")
		ready <- struct{}{}
		<-ctx.Done()
		return -1, ctx.Err()
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
	if len(got.Output) != store.RemoteJobOutputLimit || !strings.HasSuffix(got.Output, "tail") || !got.Truncated {
		t.Fatalf("tail: %d %v", len(got.Output), got.Truncated)
	}
}

func TestCrashAfterAcceptanceNeverExecutesAgain(t *testing.T) {
	st := storetest.Clone(t)
	r := store.RemoteJob{ID: uuid.NewString(), OwnerID: "owner", SourceThreadID: uuid.NewString(), ProjectID: uuid.NewString(), Workspace: t.TempDir(), Fingerprint: strings.Repeat("a", 64)}
	if _, fresh, err := st.AcceptRemoteJob(r); err != nil || !fresh {
		t.Fatalf("accept: %v %v", fresh, err)
	}
	m, err := New(context.Background(), st, func(context.Context, string, []string, io.Writer) (int, error) {
		t.Error("recovered command executed")
		return 0, nil
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
	m, _ := manager(t, func(context.Context, string, []string, io.Writer) (int, error) {
		t.Error("invalid request executed")
		return 0, nil
	})
	for _, mutate := range []func(*Request){
		func(r *Request) { r.ID = "../bad" }, func(r *Request) { r.SourceThreadID = "" }, func(r *Request) { r.Argv = nil },
		func(r *Request) { r.Argv = []string{"cmd", "a\x00b"} }, func(r *Request) { r.Argv = []string{strings.Repeat("x", 64<<10+1)} },
		func(r *Request) { r.TimeoutSeconds = 0 }, func(r *Request) { r.TimeoutSeconds = MaxTimeoutSeconds + 1 },
	} {
		r := request()
		mutate(&r)
		if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}
