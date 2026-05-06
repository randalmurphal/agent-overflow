package design

import (
	"context"
	"fmt"

	"agent-overflow/internal/screenshot"
)

// Capturer is the indirection between Reactor and the headless
// browser. Implementations turn a thread id into a full-page PNG of
// what the user's design preview iframe currently renders.
//
// The interface lives in the design package (rather than in
// internal/screenshot) so the Reactor can be wired in tests with a
// trivial fake — no chromedp required. The production implementation
// lives in app.go and builds the URL from the transport listener
// address plus the per-thread workdir convention.
type Capturer interface {
	Capture(ctx context.Context, threadID string) ([]byte, error)
}

// Reactor is the unified design-mode dispatch surface for both
// providers. After the v44 rewrite the screenshot path is
// backend-driven (headless Chromium via chromedp) so there's no
// in-flight blocking state to manage on the design side; diagnostics
// remain a per-thread ring buffer. The Reactor stays as a single
// named handle so callers (Codex MCP, Claude MCP) don't have to
// thread the helpers separately.
type Reactor struct {
	Diagnostics *DiagnosticBuffer
	Capturer    Capturer
}

// NewReactor wires the helpers into a single dispatch handle.
// capturer may be nil during early boot or in tests that don't
// exercise read_screenshot — CaptureScreenshot returns a clear
// "screenshot capture unavailable" error in that case.
func NewReactor(diags *DiagnosticBuffer, capturer Capturer) *Reactor {
	return &Reactor{Diagnostics: diags, Capturer: capturer}
}

// GetDiagnostics is the backend half of the get_design_diagnostics MCP
// tool. Returns new diagnostics since the caller's last token plus the
// new highest token to round-trip.
func (r *Reactor) GetDiagnostics(ctx context.Context, threadID string, since int64) ([]Diagnostic, int64, error) {
	if r == nil || r.Diagnostics == nil {
		return nil, since, fmt.Errorf("design: diagnostics buffer unavailable")
	}
	diags, latest := r.Diagnostics.Drain(ctx, threadID, since)
	return diags, latest, nil
}

// CaptureScreenshot is the backend half of the read_screenshot MCP
// tool. Captures a full-page PNG via the injected Capturer (which
// drives a headless Chromium instance), then slices it into JPEG
// tiles bounded by the agent's per-image vision-token budget.
//
// Cancellation: ctx is the inbound MCP request context. Canceling it
// cancels the headless capture; on session teardown the MCP server
// closes its handler context which propagates here, so we don't need
// a separate per-thread cancel registry.
func (r *Reactor) CaptureScreenshot(ctx context.Context, threadID string) (CaptureResult, error) {
	if r == nil || r.Capturer == nil {
		return CaptureResult{}, fmt.Errorf("design: screenshot capture unavailable")
	}
	pngBytes, err := r.Capturer.Capture(ctx, threadID)
	if err != nil {
		return CaptureResult{}, err
	}
	res, err := screenshot.SliceTiles(pngBytes, screenshot.SliceOptions{
		// Pass MaxScreenshotTiles explicitly so the design-package
		// contract (the MCP layer's "clipped" trailer kicks in at this
		// boundary) drives the slicer rather than the slicer's defaults
		// happening to match.
		MaxTiles: MaxScreenshotTiles,
	})
	if err != nil {
		return CaptureResult{}, fmt.Errorf("design: slice tiles: %w", err)
	}
	return CaptureResult{Tiles: res.Tiles, Clipped: res.Clipped}, nil
}

// TeardownThread releases per-thread state held inside the design
// package. Today the only thread-keyed state is the diagnostic ring;
// captures are request-scoped and don't keep any.
func (r *Reactor) TeardownThread(threadID string) {
	if r == nil {
		return
	}
	if r.Diagnostics != nil {
		r.Diagnostics.TeardownThread(threadID)
	}
}
