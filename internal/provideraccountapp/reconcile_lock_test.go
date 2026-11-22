package provideraccountapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// A caller bounded by a deadline stops waiting for the provider's account
// lock at the deadline, and says what it was waiting to do: both stages of a
// usage read and both transfer account checks.
func TestBoundedCallersStopWaitingForTheAccountLockAtTheirDeadline(t *testing.T) {
	claudeName, codexName := string(provider.Claude), string(provider.Codex)
	for name, stage := range map[string]struct {
		provider string
		call     func(m *Manager, ctx context.Context) error
		names    string
	}{
		"reconcile": {
			provider: claudeName,
			call: func(m *Manager, ctx context.Context) error {
				return m.ReconcileExternalProviderAccountContext(ctx, claudeName)
			},
			names: "wait for the claude account lock to check for an outside login",
		},
		"refresh": {
			provider: claudeName,
			call: func(m *Manager, ctx context.Context) error {
				return m.RefreshProviderAccountUsageContext(ctx, claudeName, "saved")
			},
			names: "wait for the claude account lock to read account saved's usage",
		},
		"codex transfer check": {
			provider: codexName,
			call: func(m *Manager, ctx context.Context) error {
				return m.CheckCodexTransferAccount(ctx, "must-not-run")
			},
			names: "wait for the codex account lock to check the transfer's Codex account",
		},
		"claude transfer check": {
			provider: claudeName,
			call: func(m *Manager, ctx context.Context) error {
				return m.CheckClaudeTransferAccount(ctx, "must-not-run")
			},
			names: "wait for the claude account lock to identify its account",
		},
	} {
		t.Run(name, func(t *testing.T) {
			manager, _, _ := newTestManager(t)
			lock := manager.reconcileMutex(stage.provider)
			lock.Lock()
			t.Cleanup(lock.Unlock)

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- stage.call(manager, ctx) }()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), stage.names) {
					t.Fatalf("%s with the lock held = %v, want the deadline naming the stage", name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s waited for the account lock past its deadline", name)
			}
		})
	}
}

// The token refresh an abandoned transcription leaves behind stops waiting
// for the account lock with the transcription, so its work admission, which
// a restart waits for, ends while the lock is still held.
func TestAnAbandonedTranscriptionsRefreshStopsWaitingForTheAccountLock(t *testing.T) {
	manager, _, refused := refusedDictationManager(t)
	var work sync.WaitGroup
	manager.deps.BeginWork = func(ctx context.Context) (func(), error) {
		work.Add(1)
		return work.Done, ctx.Err()
	}
	lock := manager.reconcileMutex(string(provider.Codex))
	lock.Lock()
	t.Cleanup(lock.Unlock)
	ctx, cancel := context.WithCancel(context.Background())
	finished := transcribeInBackground(ctx, manager)
	<-refused
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("TranscribeWithCodexLogin err = %v, want context.Canceled", err)
	}

	admissions := make(chan struct{})
	go func() {
		work.Wait()
		close(admissions)
	}()
	select {
	case <-admissions:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh an abandoned transcription left behind kept waiting for the account lock")
	}
}

// A usage read whose bound ends after it took the account lock starts no
// identity probe for the outside login it found. The poisoned binary's
// tripwire fails the test if the probe starts.
func TestAnOutsideLoginsProbeDoesNotStartAfterItsCallerStopped(t *testing.T) {
	manager, _, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	outside := claudeLoginAs("outside@example.com", "outside", time.Now().Add(20*24*time.Hour))
	if err := credentials.WriteNativeCredentialForTest(claudeName, outside); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolve := manager.deps.ProviderBinary
	manager.deps.ProviderBinary = func(providerName string) string {
		// Resolved with the lock held, just before the probe would start.
		cancel()
		return resolve(providerName)
	}

	err := manager.ReconcileExternalProviderAccountContext(ctx, claudeName)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stopped before starting the claude identity probe") {
		t.Fatalf("reconcile after its caller stopped = %v, want the cancellation naming the probe it did not start", err)
	}
}

// The retry after the login rotated under the first probe is a new probe
// start, so it does not run once the caller's bound has ended; the first
// probe ran to its end on the manager's lifetime.
func TestTheProbeRetryDoesNotStartAfterItsCallerStopped(t *testing.T) {
	manager, _, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	sessionEnd := time.Now().Add(20 * 24 * time.Hour)
	if err := credentials.WriteNativeCredentialForTest(claudeName, claudeLoginAs("current@example.com", "first", sessionEnd)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probes := 0
	_, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: claudeName,
		Context:      ctx,
		Probe: func(life context.Context) (provider.AccountInfo, error) {
			probes++
			cancel()
			if life.Err() != nil {
				t.Errorf("the started probe's context ended with its caller's: %v", life.Err())
			}
			rotated := claudeLoginAs("current@example.com", "rotated", sessionEnd)
			return provider.AccountInfo{Email: "current@example.com"},
				credentials.WriteNativeCredentialForTest(claudeName, rotated)
		},
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stopped before starting the claude identity probe") {
		t.Fatalf("probe after the login rotated and its caller stopped = %v, want the cancellation naming the probe it did not start", err)
	}
	if probes != 1 {
		t.Fatalf("probes started = %d, want only the one started before the caller stopped", probes)
	}
}

// The lock is still a mutex for every caller that waits without a bound.
func TestReconcileLockExcludesAndHandsOver(t *testing.T) {
	var lock reconcileLock
	lock.Lock()
	ended, end := context.WithCancel(context.Background())
	end()
	if err := lock.LockContext(ended); !errors.Is(err, context.Canceled) {
		t.Fatalf("LockContext on a held lock with an ended context = %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		lock.Lock()
		close(acquired)
	}()
	lock.Unlock()
	<-acquired
	lock.Unlock()
	if err := lock.LockContext(context.Background()); err != nil {
		t.Fatalf("LockContext on a free lock = %v", err)
	}
	lock.Unlock()
}

type blockingQuotaRoundTripper struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingQuotaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(bytes.NewBufferString(
			`{"buckets":[{"bucketId":"gemini-argon-5h","window":"5h","remainingFraction":0.8}]}`,
		)),
	}, nil
}

// A read-only Antigravity HTTP quota probe must NOT hold reconcileMutex while
// waiting on the network, so account switches and sibling checks are never stalled.
func TestAgyReadOnlyUsageRefreshDoesNotHoldReconcileLock(t *testing.T) {
	manager, _, credentials := newTestManager(t)
	agyName := string(provider.Agy)
	cred := []byte(`{"access_token":"ya29.valid","refresh_token":"1//ref","id_token_email":"dev@example.com"}`)
	if err := credentials.WriteNativeCredentialForTest(agyName, cred); err != nil {
		t.Fatalf("write agy native credential: %v", err)
	}
	if _, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: agyName,
		Context:      context.Background(),
		Probe: func(context.Context) (provider.AccountInfo, error) {
			return provider.AccountInfo{Email: "dev@example.com"}, nil
		},
	}); err != nil {
		t.Fatalf("seed agy account: %v", err)
	}
	selection := manager.Selection(agyName)
	if selection.AccountID == "" {
		t.Fatalf("expected seeded active agy account, got empty selection: %+v", selection)
	}

	rt := &blockingQuotaRoundTripper{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager.deps.HTTPClient = func() *http.Client {
		return &http.Client{Transport: rt}
	}

	done := make(chan error, 1)
	go func() {
		done <- manager.RefreshProviderAccountUsageContext(context.Background(), agyName, selection.AccountID)
	}()

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agy quota HTTP request to start")
	}

	// While the HTTP request is blocked in-flight, reconcileMutex MUST be free.
	lock := manager.reconcileMutex(agyName)
	lockCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := lock.LockContext(lockCtx); err != nil {
		close(rt.release)
		t.Fatalf("reconcileMutex was held during read-only agy HTTP probe: %v", err)
	}
	lock.Unlock()

	close(rt.release)
	if err := <-done; err != nil {
		t.Fatalf("RefreshProviderAccountUsageContext(agy): %v", err)
	}
}
