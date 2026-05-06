package screenshot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
)

// errInboundCanceled is the cancellation cause stamped onto a
// capture's mergedCtx when the inbound MCP request goes away.
// `context.Cause(mergedCtx)` returning this lets us tell apart
// "agent timed out / aborted the call" from "chromedp tab or browser
// died" — both surface as context.Canceled on err.Err() but only
// the inbound case carries this cause.
var errInboundCanceled = errors.New("inbound MCP request canceled")

// prefixedLogWriter writes each newline-terminated chunk to log.Printf
// with a distinguishing prefix. Used to capture chrome-headless-shell's
// own stdout/stderr (post-wsURL handshake) so a chrome-side crash
// message lands in launcher.log alongside our screenshot: lines.
type prefixedLogWriter struct {
	prefix string
}

func (w prefixedLogWriter) Write(p []byte) (int, error) {
	// chrome can write multi-line chunks. Split on newlines so each
	// log.Printf call gets one logical line, but tolerate trailing
	// fragments by joining what's left to whatever comes next.
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(text, "\n") {
		log.Printf("%s: %s", w.prefix, line)
	}
	return len(p), nil
}

// Manager owns a single long-lived chromedp browser. The browser
// process is launched lazily on the first Capture call (so a thread
// that never asks for a screenshot pays no startup cost) and stays
// alive for the rest of the app process. Per-capture state isolation
// comes from creating a fresh chromedp.NewContext for each Capture —
// chromedp tears down the inner Target at scope exit so listeners
// and inflight requests from the previous capture don't bleed into
// the next one. We do NOT spin up a fresh BrowserContext (cookies /
// localStorage / serviceWorkers are shared), which is fine for the
// current trust model: every capture loads a loopback /design/
// URL.
//
// Manager is safe for concurrent calls. The first concurrent Capture
// pays the install + start cost; subsequent callers wait on the
// startup mutex and then proceed in parallel. A failed install does
// NOT permanently brick the Manager — the next Capture call retries
// from scratch.
type Manager struct {
	installer *Installer

	startMu sync.Mutex
	started bool

	// stateMu guards the lifetime fields below. Capture takes a brief
	// read of browserCtx under stateMu so a Close racing a Capture
	// can't read a half-cleared pointer.
	stateMu       sync.Mutex
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
}

// NewManager constructs a Manager with the supplied installer. The
// installer can be reused across Manager instances; it has no live
// state of its own.
func NewManager(installer *Installer) *Manager {
	return &Manager{installer: installer}
}

// CaptureOptions configures a single Capture call.
//
// URL is required and must be loadable by the headless browser
// (typically the loopback design file server). ViewportWidth and
// ViewportHeight describe the css-pixel viewport before
// captureBeyondViewport — the headless renderer paints at this
// size and FullScreenshot extends downward without changing the
// width. DeviceScaleFactor of 0 means "use 1.0" (1× DPR); raise it
// for hi-dpi captures, but mind that 2× quadruples the byte count.
type CaptureOptions struct {
	URL                string
	ViewportWidth      int
	ViewportHeight     int
	DeviceScaleFactor  float64
	NavigationDeadline time.Duration // 0 → 30s default
}

// Capture loads opts.URL in the headless browser, races
// document.fonts.ready against a 4 s soft cap, scrolls the document
// to settle lazy content, and returns a full-page PNG.
func (m *Manager) Capture(ctx context.Context, opts CaptureOptions) ([]byte, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("screenshot: capture URL required")
	}
	if opts.ViewportWidth <= 0 {
		opts.ViewportWidth = DefaultTileWidth
	}
	if opts.ViewportHeight <= 0 {
		opts.ViewportHeight = DefaultTileHeight
	}
	if opts.DeviceScaleFactor <= 0 {
		opts.DeviceScaleFactor = 1.0
	}
	if opts.NavigationDeadline <= 0 {
		opts.NavigationDeadline = 30 * time.Second
	}

	if err := m.ensureStarted(ctx); err != nil {
		return nil, err
	}

	// Snapshot browserCtx under stateMu so a concurrent Close can't
	// race a nil-write between our read and use. We don't hold the
	// mutex during the chromedp.Run — that would serialise captures.
	m.stateMu.Lock()
	browserCtx := m.browserCtx
	m.stateMu.Unlock()
	if browserCtx == nil {
		return nil, fmt.Errorf("screenshot: manager closed")
	}

	// Per-capture context — chromedp creates a fresh Target so
	// listeners / inflight requests don't bleed into the next call.
	captureCtx, captureCancel := chromedp.NewContext(browserCtx)
	defer captureCancel()

	deadlineCtx, deadlineCancel := context.WithTimeout(captureCtx, opts.NavigationDeadline)
	defer deadlineCancel()

	// Caller cancellation must propagate. ctx is the inbound MCP
	// request context; chaining lets a tool-call timeout abort the
	// whole capture chain. We use WithCancelCause so the inbound vs.
	// chromedp-internal cancellation paths can be told apart at the
	// top of the error chain via context.Cause(mergedCtx) — both
	// surface as context.Canceled on err.Err() otherwise.
	mergedCtx, mergedCancelCause := context.WithCancelCause(deadlineCtx)
	stop := context.AfterFunc(ctx, func() {
		mergedCancelCause(fmt.Errorf("%w: %w", errInboundCanceled, ctx.Err()))
	})
	defer func() {
		stop()
		mergedCancelCause(nil)
	}()

	captureStart := time.Now()
	browserDeadAtEntry := browserCtx.Err() != nil
	prog := &runProgress{}
	pngBytes, err := runCapture(mergedCtx, opts, prog)
	if err != nil {
		// Build a categorical diagnostic and embed it in the wrapped
		// error string. The agent's read_screenshot tool surfaces this
		// verbatim, so the user sees the trigger directly in chat
		// without needing to find the right log destination.
		detail := classifyCaptureFailure(captureFailureContext{
			elapsed:            time.Since(captureStart),
			lastStep:           prog.last(),
			mergedCause:        context.Cause(mergedCtx),
			inboundErr:         ctx.Err(),
			deadlineErr:        deadlineCtx.Err(),
			captureCtxErr:      captureCtx.Err(),
			browserErr:         browserCtx.Err(),
			browserDeadAtEntry: browserDeadAtEntry,
		})
		log.Printf("screenshot: capture %s failed: %v [%s]", opts.URL, err, detail)
		return nil, fmt.Errorf("screenshot: capture %s: %w [%s]", opts.URL, err, detail)
	}
	return pngBytes, nil
}

// captureFailureContext bundles the snapshot of all relevant ctx
// errors and the last-attempted action so classifyCaptureFailure can
// produce one categorical detail string.
type captureFailureContext struct {
	elapsed            time.Duration
	lastStep           string
	mergedCause        error
	inboundErr         error
	deadlineErr        error
	captureCtxErr      error
	browserErr         error
	browserDeadAtEntry bool
}

// classifyCaptureFailure picks the most informative single trigger
// label and packs it with the supporting detail. The four well-known
// root causes (inbound MCP cancel, our 30 s deadline, chromedp tab
// died, browser process died) are each given a distinct label so a
// reader can act without staring at five context.Err() values.
func classifyCaptureFailure(c captureFailureContext) string {
	parts := []string{fmt.Sprintf("elapsed=%s", c.elapsed.Round(time.Millisecond))}
	if c.lastStep != "" {
		parts = append(parts, "last_step="+c.lastStep)
	}
	switch {
	case c.browserDeadAtEntry:
		parts = append(parts, "trigger=browser_dead_at_entry",
			"browser_err="+errString(c.browserErr))
	case errors.Is(c.mergedCause, errInboundCanceled):
		parts = append(parts, "trigger=agent_canceled_request",
			"inbound_err="+errString(c.inboundErr))
	case errors.Is(c.deadlineErr, context.DeadlineExceeded):
		parts = append(parts, "trigger=screenshot_30s_deadline_exceeded")
	case c.browserErr != nil:
		parts = append(parts, "trigger=browser_died_during_capture",
			"browser_err="+errString(c.browserErr))
	case c.captureCtxErr != nil:
		parts = append(parts, "trigger=chromedp_tab_died",
			"capture_err="+errString(c.captureCtxErr))
	default:
		parts = append(parts, "trigger=unknown",
			"merged_cause="+errString(c.mergedCause))
	}
	return strings.Join(parts, "; ")
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// Prime kicks off install + browser boot ahead of the first Capture.
// Callers use this to move the chrome-headless-shell download (~150
// MB on first run) and chromedp handshake off the hot path of the
// agent's first read_screenshot call. Idempotent — once a Manager is
// started, Prime is a no-op. A failure here does NOT poison the
// Manager; the next Capture retries.
func (m *Manager) Prime(ctx context.Context) error {
	return m.ensureStarted(ctx)
}

// ensureStarted installs and launches the browser, or reuses an
// existing one if it's still alive. If the previously-booted browser
// died on us (chromedp's allocator marks browserCtx canceled when the
// Chrome process exits), we tear down the stale state and reboot
// transparently — Capture sees a working browserCtx either way.
//
// Multiple concurrent callers serialise on startMu; only the winner
// runs install/boot. A failed boot does NOT permanently poison the
// Manager — m.started stays false and the next call retries.
func (m *Manager) ensureStarted(ctx context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	if m.started {
		// Health check: if the long-lived browserCtx is canceled the
		// Chrome process is dead. Without this branch we'd happily
		// hand out the dead context to Capture and chromedp.Run
		// would fall through with context.Canceled in milliseconds —
		// the "errors basically immediately" symptom we chased
		// before this fix landed.
		m.stateMu.Lock()
		var bcErr error
		var bcCause error
		if m.browserCtx != nil {
			bcErr = m.browserCtx.Err()
			bcCause = context.Cause(m.browserCtx)
		}
		m.stateMu.Unlock()
		if bcErr == nil {
			return nil
		}
		log.Printf("screenshot: ensureStarted: previous browser died (err=%v cause=%v); rebooting",
			bcErr, bcCause)
		m.teardownLocked()
		m.started = false
	}

	if err := m.startLocked(ctx); err != nil {
		return err
	}
	m.started = true
	return nil
}

// teardownLocked clears the live browser/alloc fields and cancels
// their contexts. Caller must hold startMu so the in-flight check in
// ensureStarted doesn't race; stateMu is taken briefly inside.
func (m *Manager) teardownLocked() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.browserCancel != nil {
		m.browserCancel()
	}
	if m.allocCancel != nil {
		m.allocCancel()
	}
	m.browserCtx = nil
	m.browserCancel = nil
	m.allocCtx = nil
	m.allocCancel = nil
}

func (m *Manager) startLocked(ctx context.Context) error {
	res, err := m.installer.Install(ctx)
	if err != nil {
		return fmt.Errorf("screenshot: install: %w", err)
	}

	// Launch chromedp's exec allocator pointed at our cached binary.
	//
	// Three non-default options carry load-bearing rationale:
	//
	//   - NoSandbox: the SUID-helper sandbox is unavailable on many
	//     WSL / container environments and our captured pages always
	//     come from a trusted loopback URL.
	//
	//   - ModifyCmdFunc(captureCmd): bypasses chromedp's Linux-only
	//     default of cmd.SysProcAttr.Pdeathsig = SIGKILL. The intent
	//     ("kill the browser if our parent dies") backfires in Go
	//     programs because Pdeathsig fires when the spawning OS
	//     THREAD is reaped, which Go's scheduler does routinely. The
	//     non-Linux build of chromedp's allocateCmdOptions is already
	//     a no-op, so this matches macOS/Windows. Browser cleanup is
	//     still correct: chromedp launches via
	//     exec.CommandContext(allocCtx), and our allocCancel() (from
	//     Manager.Close / teardownLocked) still triggers a proper
	//     Process.Kill via os/exec. The same hook captures the
	//     *exec.Cmd into a closure-local variable for the
	//     death-watcher's post-mortem aliveness check below.
	//     Reference: https://github.com/golang/go/issues/27505.
	//
	//   - CombinedOutput: pipes chrome-headless-shell's own
	//     stdout/stderr (post-wsURL handshake) into log.Printf so a
	//     real chrome crash leaves a trace (segfault, OOM message,
	//     missing-library line, etc.). Without this, chrome dies
	//     silently from our perspective.
	var chromeCmd *exec.Cmd
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(res.BinaryPath),
		chromedp.NoSandbox,
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) { chromeCmd = cmd }),
		chromedp.CombinedOutput(prefixedLogWriter{prefix: "chrome-headless-shell"}),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)

	// chromedp.WithErrorf forwards chromedp's own error logging
	// (websocket disconnects, CDP-level decode errors) into our log
	// stream so a future browser-side fault carries a precise reason
	// instead of an opaque context.Canceled.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithErrorf(func(format string, args ...any) {
			log.Printf("chromedp: "+format, args...)
		}),
	)

	// CRITICAL: pass browserCtx (not a timeout-wrapped child) to the
	// first chromedp.Run. From chromedp.go:313-315 (v0.15.1):
	//
	//   "Note that the first time Run is called on a context, a
	//   browser will be allocated via Allocator. Thus, it's generally
	//   a bad idea to use a context timeout on the first Run call,
	//   as it will stop the entire browser."
	//
	// chromedp's Allocate launches chrome via exec.CommandContext,
	// tying the subprocess's lifetime to whatever ctx is passed to
	// this first Run. A context.WithTimeout here would defer-cancel
	// when startLocked returns, Go's os/exec reaper would SIGKILL
	// chrome milliseconds later, and every subsequent Capture would
	// see a dead browser. This was the actual cause of the
	// "browser_dead_at_entry" failure mode the diagnostic
	// instrumentation chased through several rounds.
	//
	// The warm-up itself is just "open the CDP connection, run no
	// actions, return on handshake complete" — sub-second on local
	// loopback. If chrome ever hangs here the user sees a slow first
	// capture, not silently broken later ones.
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("screenshot: launch: %w", err)
	}

	// Death watcher: log the moment browserCtx is canceled and
	// inspect the chrome subprocess at that instant. With the
	// warmCtx-fix landed, this should fire only on Manager.Close /
	// teardownLocked (clean) or on a real chrome crash (rare). The
	// liveness probe distinguishes "chrome process exited" from
	// "chromedp dropped the websocket on a healthy chrome" — the
	// latter would be a new, separate bug.
	go func() {
		<-browserCtx.Done()
		liveness := "chrome state unknown (cmd not captured)"
		if chromeCmd != nil && chromeCmd.Process != nil {
			// Send signal 0 to test process aliveness without
			// affecting it. ESRCH means the process is gone.
			if err := chromeCmd.Process.Signal(syscall.Signal(0)); err != nil {
				liveness = fmt.Sprintf("chrome pid=%d gone (signal 0 err=%v)",
					chromeCmd.Process.Pid, err)
			} else {
				liveness = fmt.Sprintf("chrome pid=%d STILL ALIVE — chromedp dropped websocket on a healthy browser",
					chromeCmd.Process.Pid)
			}
			if chromeCmd.ProcessState != nil {
				liveness += fmt.Sprintf(" (ProcessState=%s)", chromeCmd.ProcessState.String())
			}
		}
		log.Printf("screenshot: long-lived browserCtx canceled: err=%v cause=%v; %s",
			browserCtx.Err(), context.Cause(browserCtx), liveness)
	}()

	m.stateMu.Lock()
	m.allocCtx = allocCtx
	m.allocCancel = allocCancel
	m.browserCtx = browserCtx
	m.browserCancel = browserCancel
	m.stateMu.Unlock()
	return nil
}

// Close tears down the browser. Safe to call on a never-started
// Manager (it's a no-op then) and idempotent under repeated calls.
func (m *Manager) Close() error {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.browserCancel != nil {
		m.browserCancel()
		m.browserCancel = nil
	}
	if m.allocCancel != nil {
		m.allocCancel()
		m.allocCancel = nil
	}
	m.browserCtx = nil
	m.allocCtx = nil
	return nil
}
