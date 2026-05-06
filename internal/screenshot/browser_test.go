package screenshot

import (
	"context"
	"strings"
	"testing"
)

// TestManagerCloseIsIdempotent pins the documented Close contract: a
// never-started Manager closes cleanly, and repeated Close calls are a
// no-op. App.Shutdown's recorder calls Close unconditionally, so a
// panic or stale-cancel here would surface there as a noisy shutdown.
func TestManagerCloseIsIdempotent(t *testing.T) {
	m := NewManager(nil)

	if err := m.Close(); err != nil {
		t.Fatalf("Close on never-started manager = %v, want nil", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestManagerCaptureRequiresURL is the front-line guard against a
// caller forgetting to set CaptureOptions.URL. The check fires before
// browser bootstrap so a misuse never costs a chrome-headless-shell
// install.
func TestManagerCaptureRequiresURL(t *testing.T) {
	m := NewManager(nil)
	defer m.Close()

	_, err := m.Capture(context.Background(), CaptureOptions{})
	if err == nil {
		t.Fatal("Capture with empty URL = nil err, want error")
	}
	if !strings.Contains(err.Error(), "URL required") {
		t.Errorf("err = %v, want it to mention URL required", err)
	}
}

// TestManagerPrimeOnStartedManagerIsNoop pins the idempotency
// contract: a Prime call on an already-started Manager returns nil
// without re-running install or boot. App fires Prime on every design
// session activation; if the second activation re-installed Chrome
// it'd defeat the entire point of priming.
func TestManagerPrimeOnStartedManagerIsNoop(t *testing.T) {
	m := NewManager(nil)
	// Pretend boot already succeeded.
	m.startMu.Lock()
	m.started = true
	m.startMu.Unlock()

	// installer is nil; if Prime mistakenly called startLocked we'd
	// nil-deref on m.installer.Install. Returning nil is the contract.
	if err := m.Prime(context.Background()); err != nil {
		t.Fatalf("Prime on started manager = %v, want nil", err)
	}
	if err := m.Prime(context.Background()); err != nil {
		t.Fatalf("second Prime = %v, want nil", err)
	}
}

// TestManagerCaptureAfterCloseStartedManager pins the post-Close
// "manager closed" branch: when a Manager has already booted (started
// flag is true and we hold a browserCtx) and is then Closed, a
// subsequent Capture returns the documented error rather than nil-
// dereferencing on the cleared browserCtx. We simulate the post-boot
// state by hand-flipping started=true and giving Close a fake cancel
// to invoke; this exercises the Capture-side check without spawning
// an actual chrome-headless-shell process.
func TestManagerCaptureAfterCloseStartedManager(t *testing.T) {
	m := NewManager(nil)

	// Pretend boot succeeded: Close clears these on the way out, so the
	// Capture on a Closed manager hits the nil-browserCtx guard.
	m.startMu.Lock()
	m.started = true
	m.startMu.Unlock()
	m.stateMu.Lock()
	m.browserCtx = context.Background() // non-nil so Close has something to clear
	m.browserCancel = func() {}
	m.allocCancel = func() {}
	m.stateMu.Unlock()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := m.Capture(context.Background(), CaptureOptions{URL: "http://127.0.0.1/"})
	if err == nil {
		t.Fatal("Capture after Close = nil err, want manager-closed error")
	}
	if !strings.Contains(err.Error(), "manager closed") {
		t.Errorf("err = %v, want it to mention manager closed", err)
	}
}
