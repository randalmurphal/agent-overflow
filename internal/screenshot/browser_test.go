package screenshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestClassifyCaptureFailure pins the categorization of the four
// well-known root causes. The labels feed back to the agent via the
// read_screenshot tool error, so a regression here would silently
// turn an actionable diagnostic into an opaque one.
func TestClassifyCaptureFailure(t *testing.T) {
	cases := []struct {
		name   string
		input  captureFailureContext
		expect string
	}{
		{
			name: "browser dead at entry",
			input: captureFailureContext{
				elapsed:            5 * time.Millisecond,
				browserDeadAtEntry: true,
				browserErr:         context.Canceled,
				lastStep:           "setMetricsOverride",
			},
			expect: "trigger=browser_dead_at_entry",
		},
		{
			name: "agent canceled request",
			input: captureFailureContext{
				elapsed:     2 * time.Second,
				mergedCause: fmt.Errorf("%w: %w", errInboundCanceled, context.Canceled),
				inboundErr:  context.Canceled,
				lastStep:    "navigate",
			},
			expect: "trigger=agent_canceled_request",
		},
		{
			name: "30s deadline exceeded",
			input: captureFailureContext{
				elapsed:     30 * time.Second,
				deadlineErr: context.DeadlineExceeded,
				lastStep:    "navigate",
			},
			expect: "trigger=screenshot_30s_deadline_exceeded",
		},
		{
			name: "browser died mid-capture",
			input: captureFailureContext{
				elapsed:    10 * time.Second,
				browserErr: context.Canceled,
				lastStep:   "settleDocument",
			},
			expect: "trigger=browser_died_during_capture",
		},
		{
			name: "chromedp tab died",
			input: captureFailureContext{
				elapsed:       3 * time.Second,
				captureCtxErr: context.Canceled,
				lastStep:      "navigate",
			},
			expect: "trigger=chromedp_tab_died",
		},
		{
			name: "unknown",
			input: captureFailureContext{
				elapsed:     1 * time.Second,
				mergedCause: errors.New("something weird"),
			},
			expect: "trigger=unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCaptureFailure(tc.input)
			if !strings.Contains(got, tc.expect) {
				t.Errorf("classifyCaptureFailure(%s) = %q, want it to contain %q",
					tc.name, got, tc.expect)
			}
			if !strings.Contains(got, "elapsed=") {
				t.Errorf("classifyCaptureFailure(%s) missing elapsed= field: %q", tc.name, got)
			}
		})
	}
}

// TestManagerEnsureStartedDetectsDeadBrowser pins the auto-reboot
// contract: a Manager whose previously-booted browser died on us
// (browserCtx canceled while m.started is still true) is detected by
// ensureStarted, torn down, and queued for re-init. Without this
// branch we'd hand out the dead context to Capture and fall out of
// chromedp.Run with context.Canceled in milliseconds — exactly the
// "browser_dead_at_entry" trigger we observed in production.
//
// The test simulates a started-then-dead Manager by hand-flipping
// state, then verifies ensureStarted with a nil installer fails on
// startLocked (proving it ATTEMPTED a reboot rather than returning
// nil from the m.started fast path).
func TestManagerEnsureStartedDetectsDeadBrowser(t *testing.T) {
	m := NewManager(nil)

	// Simulate a previously-booted browser that died.
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel() // immediately canceled — represents a dead browser

	m.startMu.Lock()
	m.started = true
	m.startMu.Unlock()
	m.stateMu.Lock()
	m.browserCtx = deadCtx
	m.browserCancel = func() {}
	m.allocCancel = func() {}
	m.stateMu.Unlock()

	// ensureStarted must NOT return nil from the started=true fast
	// path; it must detect the dead browser, tear down, and try to
	// reboot via startLocked. A nil installer panics on Install ->
	// catch the panic to confirm we reached startLocked.
	defer func() {
		// recover the nil-deref from m.installer.Install — that's our
		// proof the rebuild path ran. Any other outcome (no panic, or
		// nil error) means ensureStarted incorrectly returned from the
		// started=true fast path.
		r := recover()
		if r == nil {
			t.Fatal("ensureStarted with dead browser returned without trying to reboot — should have called startLocked and panicked on nil installer")
		}
		// Confirm the teardown ran by checking state is cleared.
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		if m.browserCtx != nil {
			t.Errorf("browserCtx not cleared during reboot teardown: %v", m.browserCtx)
		}
	}()
	_ = m.ensureStarted(context.Background())
}

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
