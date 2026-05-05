package design

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CaptureMaxWait caps how long Capture will block before declaring the
// frontend stuck. The MCP request context normally enforces its own
// deadline, but a Codex/Claude binary configured with no tool timeout
// would otherwise leave the goroutine alive forever if the frontend
// went silent.
const CaptureMaxWait = 30 * time.Second

// ErrScreenshotSessionEnded is returned when a pending screenshot
// request is cancelled because the design session was torn down.
var ErrScreenshotSessionEnded = errors.New("design screenshot session ended")

type pendingScreenshot struct {
	threadID string
	resultCh chan screenshotResolution
}

type screenshotResolution struct {
	png []byte
	err error
}

// ScreenshotBroker serializes the round-trip between the agent's
// `read_screenshot` MCP tool call and the frontend's iframe capture.
// Backend pushes a ScreenshotRequest event; frontend captures and
// posts back via Resolve. Mirrors the design.Reactor blocking pattern.
type ScreenshotBroker struct {
	emit func(eventName string, data any)

	mu      sync.Mutex
	pending map[string]*pendingScreenshot
}

// NewScreenshotBroker constructs a broker. emit fires the
// design:capture-request event the frontend listens for; passing nil
// turns Capture into a hard error.
func NewScreenshotBroker(emit func(eventName string, data any)) *ScreenshotBroker {
	return &ScreenshotBroker{
		emit:    emit,
		pending: make(map[string]*pendingScreenshot),
	}
}

// CaptureEventName is the event the frontend subscribes to. Centralized
// here so callers don't risk typos; the event payload is
// design.ScreenshotRequest.
const CaptureEventName = "design:capture-request"

// Capture issues a screenshot request and blocks until the frontend
// responds, the session ends, or ctx is cancelled. Returns the PNG
// bytes the frontend captured.
func (b *ScreenshotBroker) Capture(ctx context.Context, threadID string) ([]byte, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("design: screenshot thread id required")
	}
	if b == nil || b.emit == nil {
		return nil, fmt.Errorf("design: screenshot broker unavailable")
	}

	requestID := uuid.NewString()
	resultCh := make(chan screenshotResolution, 1)

	b.mu.Lock()
	b.pending[requestID] = &pendingScreenshot{
		threadID: threadID,
		resultCh: resultCh,
	}
	b.mu.Unlock()

	b.emit(CaptureEventName, ScreenshotRequest{
		ThreadID:  threadID,
		RequestID: requestID,
	})

	timer := time.NewTimer(CaptureMaxWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		b.cancel(requestID, fmt.Errorf("design: screenshot cancelled: %w", ctx.Err()))
		return nil, ctx.Err()
	case <-timer.C:
		b.cancel(requestID, fmt.Errorf("design: screenshot timed out after %s", CaptureMaxWait))
		return nil, fmt.Errorf("design: screenshot timed out after %s", CaptureMaxWait)
	case resolution := <-resultCh:
		return resolution.png, resolution.err
	}
}

// Resolve completes a pending screenshot request. requestID is the
// opaque uuid the broker minted when Capture was called; thread
// validation is unnecessary because the id is unique across all
// in-flight requests for the lifetime of the broker.
func (b *ScreenshotBroker) Resolve(requestID string, png []byte) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("design: screenshot requestID required")
	}

	b.mu.Lock()
	pending, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("design: screenshot request %s not found", requestID)
	}
	select {
	case pending.resultCh <- screenshotResolution{png: png}:
	default:
	}
	return nil
}

// Fail marks a pending screenshot request as failed. Used when the
// frontend's capture itself errored (e.g. iframe not ready).
func (b *ScreenshotBroker) Fail(requestID string, reason string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("design: screenshot requestID required")
	}
	if reason == "" {
		reason = "capture failed"
	}

	b.mu.Lock()
	pending, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("design: screenshot request %s not found", requestID)
	}
	select {
	case pending.resultCh <- screenshotResolution{err: errors.New(reason)}:
	default:
	}
	return nil
}

// TeardownThread cancels every pending screenshot for a thread.
// Mirrors design.Reactor.TeardownThread so a session ending while a
// screenshot is mid-flight doesn't leak the goroutine.
func (b *ScreenshotBroker) TeardownThread(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}

	var pending []*pendingScreenshot
	b.mu.Lock()
	for requestID, p := range b.pending {
		if p.threadID != threadID {
			continue
		}
		delete(b.pending, requestID)
		pending = append(pending, p)
	}
	b.mu.Unlock()

	for _, p := range pending {
		select {
		case p.resultCh <- screenshotResolution{err: ErrScreenshotSessionEnded}:
		default:
		}
	}
}

// cancel resolves a pending request with an error and removes it from
// the map. Used by the ctx-cancelled branch of Capture.
func (b *ScreenshotBroker) cancel(requestID string, err error) {
	b.mu.Lock()
	pending, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()

	if !ok {
		return
	}
	select {
	case pending.resultCh <- screenshotResolution{err: err}:
	default:
	}
}
