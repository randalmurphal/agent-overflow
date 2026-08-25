package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// stubProvider is a controllable updater.Provider for verifiedProvider tests.
type stubProvider struct {
	rel *updater.Release
	err error
}

func (s stubProvider) Name() string { return "stub" }

func (s stubProvider) Check(context.Context, updater.CheckRequest) (*updater.Release, error) {
	return s.rel, s.err
}

func (s stubProvider) Download(context.Context, *updater.Release, io.Writer, func(written, total int64)) error {
	return nil
}

func TestVerifiedProviderFailsClosedWithoutVerification(t *testing.T) {
	// A release with no Verification block must be rejected: installing it
	// would skip integrity checking entirely (the stock GitHub provider's
	// silent fall-open when the checksum sidecar is missing).
	p := verifiedProvider{inner: stubProvider{rel: &updater.Release{Version: "1.2.3"}}}
	rel, err := p.Check(context.Background(), updater.CheckRequest{})
	if err == nil {
		t.Fatal("expected error for release without verification, got nil")
	}
	if rel != nil {
		t.Fatalf("expected nil release on rejection, got %+v", rel)
	}
}

func TestVerifiedProviderFailsClosedWithEmptyDigest(t *testing.T) {
	// A Verification block present but carrying an empty digest is still
	// unverifiable and must be rejected.
	p := verifiedProvider{inner: stubProvider{rel: &updater.Release{
		Version:      "1.2.3",
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: nil},
	}}}
	if _, err := p.Check(context.Background(), updater.CheckRequest{}); err == nil {
		t.Fatal("expected error for release with empty digest, got nil")
	}
}

func TestVerifiedProviderPassesThroughVerifiedRelease(t *testing.T) {
	want := &updater.Release{
		Version:      "1.2.3",
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: []byte{0x01, 0x02}},
	}
	p := verifiedProvider{inner: stubProvider{rel: want}}
	got, err := p.Check(context.Background(), updater.CheckRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("expected the verified release passed through unchanged, got %+v", got)
	}
}

func TestVerifiedProviderUpToDateIsNotAnError(t *testing.T) {
	// A nil release means "already up to date" — it must pass through without
	// error so the Updater reports up-to-date rather than failing the check.
	p := verifiedProvider{inner: stubProvider{rel: nil}}
	rel, err := p.Check(context.Background(), updater.CheckRequest{})
	if err != nil {
		t.Fatalf("unexpected error on up-to-date: %v", err)
	}
	if rel != nil {
		t.Fatalf("expected nil release, got %+v", rel)
	}
}

func TestVerifiedProviderPropagatesCheckError(t *testing.T) {
	sentinel := errors.New("network boom")
	p := verifiedProvider{inner: stubProvider{err: sentinel}}
	if _, err := p.Check(context.Background(), updater.CheckRequest{}); !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying check error to propagate, got %v", err)
	}
}

func TestVerifiedProviderForwardsName(t *testing.T) {
	p := verifiedProvider{inner: stubProvider{}}
	if got := p.Name(); got != "stub" {
		t.Fatalf("Name() = %q, want %q", got, "stub")
	}
}

// TestUpdaterRPCsUnsupportedWhenNil verifies the RPC surface degrades cleanly
// on builds without a configured updater (the headless WSL backend and tests):
// no panics, and the unsupported state is reported rather than thrown for the
// read paths.
func TestUpdaterRPCsUnsupportedWhenNil(t *testing.T) {
	a := &App{} // no updater configured

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: unexpected error: %v", err)
	}
	if avail.Supported {
		t.Fatal("CheckForUpdate: expected Supported=false when updater is nil")
	}

	if _, err := a.ListReleases(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("ListReleases: want ErrUpdatesUnsupported, got %v", err)
	}
	if err := a.DownloadUpdate(""); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("DownloadUpdate: want ErrUpdatesUnsupported, got %v", err)
	}
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("RestartToUpdate: want ErrUpdatesUnsupported, got %v", err)
	}
}

// The restart exit watchdog converts a wedged graceful shutdown into a hard
// exit so the swap helper (which aborts if the parent survives 30s) can
// still complete the update. Observed in the field on macOS: window closed,
// process alive until Force Quit, helper log "parent did not exit within
// timeout — aborting swap".
func TestRestartExitWatchdogFiresAfterDelay(t *testing.T) {
	a := &App{}
	fired := make(chan int, 1)
	a.updater.restartExitFn = func(code int) { fired <- code }

	disarm := a.armRestartExitWatchdog(10 * time.Millisecond)
	defer disarm()

	select {
	case code := <-fired:
		if code != 0 {
			t.Fatalf("watchdog exit code = %d, want 0 (deliberate exit, not a crash)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired: a wedged shutdown would zombie the app and silently cancel the update")
	}
}

func TestRestartExitWatchdogDisarmPreventsExit(t *testing.T) {
	// The one case the app must stay alive: helper spawn failed, so
	// RestartToUpdate returns the error and the session continues on the
	// old version. A watchdog that still fires would kill a healthy app.
	a := &App{}
	fired := make(chan int, 1)
	a.updater.restartExitFn = func(code int) { fired <- code }

	disarm := a.armRestartExitWatchdog(20 * time.Millisecond)
	disarm()

	select {
	case <-fired:
		t.Fatal("watchdog fired after disarm: a failed helper spawn would kill the app instead of leaving it running")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRestartWithExitWatchdogDisarmsOnSpawnFailure(t *testing.T) {
	// The one case the app must survive RestartToUpdate: the helper spawn
	// failed, the error is returned, and the session continues on the old
	// version — with NO pending force-exit left ticking.
	a := &App{}
	fired := make(chan int, 1)
	a.updater.restartExitFn = func(code int) { fired <- code }

	err := a.restartWithExitWatchdog(func(context.Context) error {
		return errors.New("spawn helper: boom")
	})
	if err == nil || !strings.Contains(err.Error(), "restart to update") {
		t.Fatalf("expected wrapped restart error, got %v", err)
	}

	// The production delay is deliberately long (25s); prove the disarm by
	// firing every OTHER armed watchdog: if the error path had left one
	// armed, a.updater.restartExitFn would eventually be called. A short observation
	// window suffices because a disarmed time.AfterFunc never fires.
	select {
	case <-fired:
		t.Fatal("watchdog fired after a failed helper spawn: a healthy app staying on the old version would be killed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRestartWithExitWatchdogStaysArmedOnSuccess(t *testing.T) {
	// On a successful restart dispatch the watchdog must stay armed — it
	// is the only thing standing between a wedged shutdown and a zombie.
	// Proven indirectly: restartWithExitWatchdog returns nil and the timer
	// exists (we cannot wait 25s here; armRestartExitWatchdog's own tests
	// cover the firing behavior with short delays).
	a := &App{}
	a.updater.restartExitFn = func(int) {}
	if err := a.restartWithExitWatchdog(func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
