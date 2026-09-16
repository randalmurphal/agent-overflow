package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/transport"
)

// TestBoundedStoreReadErrorClassifiesOnlyExpiredDeadlines pins the rule every
// bounded read shares: an expired deadline is retryable and becomes
// `temporarily_unavailable`; a cancellation (the caller went away) and an
// ordinary store failure are not, and keep their own cause.
func TestBoundedStoreReadErrorClassifiesOnlyExpiredDeadlines(t *testing.T) {
	storeErr := errors.New("sql: Rows are closed")

	ordinary := boundedStoreReadError(context.Background(), "thread defaults", threadDefaultsTimeout, storeErr)
	if errors.Is(ordinary, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("ordinary store error was mislabeled transient: %v", ordinary)
	}
	if !errors.Is(ordinary, storeErr) {
		t.Fatalf("ordinary store error lost its cause: %v", ordinary)
	}

	canceled, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()
	if err := boundedStoreReadError(canceled, "thread defaults", threadDefaultsTimeout, storeErr); errors.Is(err, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("canceled read was mislabeled as a timeout: %v", err)
	}

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancelExpired()
	transient := boundedStoreReadError(expired, "thread defaults", threadDefaultsTimeout, storeErr)
	if !errors.Is(transient, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("expired read = %v, want transient classification", transient)
	}
	if !errors.Is(transient, context.DeadlineExceeded) {
		t.Fatalf("expired read lost its context cause: %v", transient)
	}
}

// contextRecordingModels is threadModelPolicy plus the context Seed was
// handed, so a test can assert what the binding put a deadline on.
type contextRecordingModels struct {
	threadapp.ModelPolicy
	seedContext context.Context
}

func (m *contextRecordingModels) Seed(ctx context.Context, providerName, model string) store.ChatModelProfile {
	m.seedContext = ctx
	return m.ModelPolicy.Seed(ctx, providerName, model)
}

// TestGetThreadDefaultsBoundsItsStoreReads pins the wiring: the binding hands
// the service a deadline rather than letting a stalled reader pool hold the
// RPC open forever. internal/store proves a blocked pool honors one, and
// internal/threadapp proves Defaults carries it into every read it makes.
func TestGetThreadDefaultsBoundsItsStoreReads(t *testing.T) {
	app := newTestAppWithStore(t)

	models := &contextRecordingModels{ModelPolicy: threadModelPolicy{app: app}}
	app.threadAppOnce.Do(func() {})
	app.threadApp = threadapp.New(threadapp.Deps{
		Store:       app.store,
		Models:      models,
		Workspace:   threadWorkspacePort{app: app},
		LifeContext: app.lifeCtx,
	})

	if _, err := app.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID}); err != nil {
		t.Fatalf("GetThreadDefaults: %v", err)
	}
	if models.seedContext == nil {
		t.Fatal("GetThreadDefaults ran without handing its context to the service")
	}
	deadline, ok := models.seedContext.Deadline()
	if !ok {
		t.Fatal("GetThreadDefaults passed a context with no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > threadDefaultsTimeout {
		t.Fatalf("deadline is %s away, want (0, %s]", remaining, threadDefaultsTimeout)
	}
}
