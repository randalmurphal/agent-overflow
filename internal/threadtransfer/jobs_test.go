package threadtransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transferwire"
)

type runnerFunc func(context.Context, string) (store.ThreadTransfer, error)

func (f runnerFunc) Run(ctx context.Context, id string) (store.ThreadTransfer, error) {
	return f(ctx, id)
}

func jobStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(storetest.ClonePath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

func createTransferJob(t *testing.T, st *store.Store, direction string) store.ThreadTransfer {
	t.Helper()
	hash := sha256.Sum256([]byte("test"))
	request := store.ThreadTransfer{ID: entityid.New(), ThreadID: entityid.New(), PeerBackendID: entityid.New(), Kind: "move", Direction: direction, ActivationHash: hex.EncodeToString(hash[:]), PrivateState: json.RawMessage("{}")}
	if direction == "incoming" {
		request.OwnershipEpoch = 1
	}
	row, err := st.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	if direction == "outgoing" {
		if _, err := st.BindThreadTransferPeer(row.ID, json.RawMessage("{}")); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := st.BindThreadTransferArchive(row.ID, transferwire.Upload{SHA256: hex.EncodeToString(hash[:]), Size: 1024}); err != nil {
			t.Fatal(err)
		}
	}
	return row
}

func nextJobSignal(t *testing.T, signal <-chan string) string {
	t.Helper()
	select {
	case id := <-signal:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("host job did not progress")
		return ""
	}
}

func testJobs(t *testing.T, st *store.Store, source, destination Runner, publish func(store.ThreadTransfer)) *Jobs {
	t.Helper()
	if publish == nil {
		publish = func(store.ThreadTransfer) {}
	}
	j, err := NewJobs(context.Background(), st, source, destination, func(error) string { return "Connection interrupted." }, publish, func(err error) { t.Errorf("job infrastructure: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.Close)
	return j
}

func TestTransferJobsBoundRecoveryAndJoinShutdown(t *testing.T) {
	st := jobStore(t)
	for range 20 {
		createTransferJob(t, st, "outgoing")
	}
	started := make(chan string, 20)
	var active, peak atomic.Int32
	runner := runnerFunc(func(ctx context.Context, id string) (store.ThreadTransfer, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for before := peak.Load(); count > before; before = peak.Load() {
			if peak.CompareAndSwap(before, count) {
				break
			}
		}
		started <- id
		<-ctx.Done()
		return store.ThreadTransfer{}, ctx.Err()
	})
	j := testJobs(t, st, runner, runner, nil)
	for range 4 {
		nextJobSignal(t, started)
	}
	j.mu.Lock()
	running := len(j.active)
	j.mu.Unlock()
	if running != 4 || peak.Load() != 4 {
		t.Fatalf("unbounded recovery: %d peak %d", running, peak.Load())
	}
	j.Close()
	if active.Load() != 0 {
		t.Fatal("shutdown returned before workers stopped")
	}
	select {
	case <-started:
		t.Fatal("shutdown dispatched another job")
	default:
	}
}

func TestTransferJobWakeDuringAttemptCannotBeOverwrittenByItsResult(t *testing.T) {
	st := jobStore(t)
	row := createTransferJob(t, st, "incoming")
	started := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	runner := runnerFunc(func(ctx context.Context, id string) (store.ThreadTransfer, error) {
		attempt := calls.Add(1)
		started <- id
		if attempt == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return store.ThreadTransfer{}, ctx.Err()
			}
		} else {
			<-ctx.Done()
			return store.ThreadTransfer{}, ctx.Err()
		}
		current, err := st.GetThreadTransfer(id)
		if err != nil {
			return current, err
		}
		return current, ErrPending
	})
	j := testJobs(t, st, runner, runner, nil)
	nextJobSignal(t, started)
	j.Wake(row.ID)
	close(release)
	if id := nextJobSignal(t, started); id != row.ID || calls.Load() != 2 {
		t.Fatal("incoming wake was lost when the previous result parked")
	}
}

func TestTransferJobsRecoverParkedIncomingWorkAfterRestart(t *testing.T) {
	st := jobStore(t)
	row := createTransferJob(t, st, "incoming")
	started := make(chan string, 2)
	runner := runnerFunc(func(ctx context.Context, id string) (store.ThreadTransfer, error) {
		started <- id
		current, err := st.GetThreadTransfer(id)
		if err != nil {
			return current, err
		}
		return current, ErrPending
	})
	j := testJobs(t, st, runner, runner, nil)
	nextJobSignal(t, started)
	deadline := time.Now().Add(3 * time.Second)
	for {
		jobs, err := st.NextThreadTransferJobs(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 && jobs[0].NextAttemptAt == math.MaxInt64 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("incoming wait was not parked")
		}
		time.Sleep(time.Millisecond)
	}
	j.Close()
	restarted := testJobs(t, st, runner, runner, nil)
	if id := nextJobSignal(t, started); id != row.ID {
		t.Fatal("restart lost parked activation recovery")
	}
	restarted.Close()
}

func TestDestinationHostJobCompletesAfterRequestContextDisappears(t *testing.T) {
	f := newDestinationFixture(t)
	progress := make(chan string, 8)
	unusedSource := runnerFunc(func(context.Context, string) (store.ThreadTransfer, error) {
		t.Error("unexpected source job")
		return store.ThreadTransfer{}, ErrPending
	})
	j := testJobs(t, f.d.store, unusedSource, f.d, func(row store.ThreadTransfer) { progress <- row.Phase })
	f.d.wake = j.Wake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(f.archive)
	if err := f.d.ReceiveChunk(ctx, f.row.ID, 0, int64(len(f.archive)), hex.EncodeToString(hash[:]), bytes.NewReader(f.archive)); err != nil {
		t.Fatal(err)
	}
	if phase := nextJobSignal(t, progress); phase != "prepared" {
		t.Fatalf("host preparation: %s", phase)
	}
	if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
		t.Fatal(err)
	}
	cancel()
	if phase := nextJobSignal(t, progress); phase != "complete" {
		t.Fatalf("host activation: %s", phase)
	}
	if err := f.d.store.CheckThreadTransferAccess(f.row.ThreadID); err != nil {
		t.Fatalf("request disconnect blocked activation: %v", err)
	}
}
