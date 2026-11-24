package harnessrpc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The call hands the dispatcher's context and the data directory to the
// build, and a cancelled context releases a build that is held.
func TestHarnessPrimeTransferTemplateRunsUnderTheCallsContext(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	h.config.DataDir = t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var parent string
	h.primeTemplate = func(ctx context.Context, dir string) error {
		parent = dir
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errors.New("held build released by test cleanup")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- h.HarnessPrimeTransferTemplate(ctx) }()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("prime returned %v without running the build", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the build never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled prime = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelling the context did not release the prime")
	}
	if parent != h.config.DataDir {
		t.Fatalf("build ran under %q, want the data directory %q", parent, h.config.DataDir)
	}
}

// A build failure reaches the caller.
func TestHarnessPrimeTransferTemplateReturnsTheBuildFailure(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	buildErr := errors.New("disk full")
	h.primeTemplate = func(context.Context, string) error { return buildErr }
	if err := h.HarnessPrimeTransferTemplate(context.Background()); !errors.Is(err, buildErr) {
		t.Fatalf("prime = %v, want the build error", err)
	}
}
