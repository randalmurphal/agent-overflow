package screenshot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

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

	startMu   sync.Mutex
	started   bool
	startErr  error

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

// Capture loads opts.URL in the headless browser, awaits
// document.fonts.ready, scrolls the document to settle lazy content,
// and returns a full-page PNG.
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
	// whole capture chain. context.AfterFunc wires the propagation
	// without spawning a long-lived goroutine.
	mergedCtx, mergedCancel := context.WithCancel(deadlineCtx)
	stop := context.AfterFunc(ctx, mergedCancel)
	defer func() {
		stop()
		mergedCancel()
	}()

	return runCapture(mergedCtx, opts)
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

// ensureStarted installs and launches the browser exactly once per
// successful boot. A failed install or boot does NOT permanently
// poison the Manager — the next call retries from scratch. Multiple
// concurrent first-callers serialise on startMu; only the winner
// runs the install.
func (m *Manager) ensureStarted(ctx context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started {
		return nil
	}
	if err := m.startLocked(ctx); err != nil {
		m.startErr = err
		return err
	}
	m.started = true
	m.startErr = nil
	return nil
}

func (m *Manager) startLocked(ctx context.Context) error {
	res, err := m.installer.Install(ctx)
	if err != nil {
		return fmt.Errorf("screenshot: install: %w", err)
	}

	// Launch chromedp's exec allocator pointed at our cached binary.
	// The flag set extends DefaultExecAllocatorOptions with our own
	// minimum-viable additions; sandbox is disabled because the
	// captured pages always come from a loopback URL the user
	// already trusts, and the SUID-helper sandbox is unavailable on
	// many WSL / container environments.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(res.BinaryPath),
		chromedp.NoSandbox,
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)

	// Run the first NewContext to actually boot the browser process.
	// We hold this top-level context for the lifetime of the Manager
	// — every per-capture context is a child.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	// Bring the browser online with a single navigate-to-blank so a
	// cold first capture doesn't pay the cdp-handshake cost inside
	// its own measured window.
	warmCtx, warmCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer warmCancel()
	if err := chromedp.Run(warmCtx); err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("screenshot: launch: %w", err)
	}

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
