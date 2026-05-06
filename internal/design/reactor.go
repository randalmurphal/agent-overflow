package design

import (
	"context"
	"fmt"
)

// Reactor is the unified design-mode dispatch surface for both
// providers. After the v42 rewrite the only blocking flow is the
// screenshot round-trip — clarification cards, slider exposure, and
// option-set notifications are non-blocking structured assistant
// messages, and feedback batches are regular user messages. The
// Reactor delegates to ScreenshotBroker and DiagnosticBuffer; it
// stays as a single named handle so callers (Codex MCP, Claude MCP)
// don't have to thread the two helpers separately.
type Reactor struct {
	Diagnostics *DiagnosticBuffer
	Screenshots *ScreenshotBroker
}

// NewReactor wires the two helpers into a single dispatch handle.
func NewReactor(diags *DiagnosticBuffer, screenshots *ScreenshotBroker) *Reactor {
	return &Reactor{Diagnostics: diags, Screenshots: screenshots}
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
// tool. Blocks until the frontend responds, the session ends, or ctx
// is cancelled. Returns one or more JPEG tiles plus a clipped flag
// indicating whether the page overran the iframe's tile budget.
func (r *Reactor) CaptureScreenshot(ctx context.Context, threadID string) (CaptureResult, error) {
	if r == nil || r.Screenshots == nil {
		return CaptureResult{}, fmt.Errorf("design: screenshot broker unavailable")
	}
	return r.Screenshots.Capture(ctx, threadID)
}

// TeardownThread releases all per-thread state (diagnostics buffer,
// pending screenshots). Called from app teardown when a session ends.
func (r *Reactor) TeardownThread(threadID string) {
	if r == nil {
		return
	}
	if r.Diagnostics != nil {
		r.Diagnostics.TeardownThread(threadID)
	}
	if r.Screenshots != nil {
		r.Screenshots.TeardownThread(threadID)
	}
}
