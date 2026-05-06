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

// MaxScreenshotTiles is the hard cap on tiles a single Resolve can
// carry. The iframe-side capture script enforces a soft cap of 8
// (server.go's MAX_TILES); the broker doubles that as headroom for
// future budget bumps, with anything beyond rejected as malformed.
// Defense-in-depth — IngestScreenshot enforces the same bound on the
// way in.
const MaxScreenshotTiles = 16

// MaxScreenshotTileBase64Bytes caps the per-tile encoded length. A
// 1280×800 JPEG-q-0.85 sits at ~150 KiB raw; 1.5 MiB of base64 (≈
// 1.1 MiB raw) gives 7× headroom for genuinely complex content while
// keeping a malformed payload bounded.
const MaxScreenshotTileBase64Bytes = 1_500_000

// MaxScreenshotTotalBase64Bytes caps the aggregate encoded payload.
// Sized so that MaxScreenshotTiles × typical-tile fits comfortably
// while a single megaslice of base64 can't pin the full transport
// frame budget on the screenshot path.
const MaxScreenshotTotalBase64Bytes = 8_000_000

// CaptureResult is the bundle the broker hands back to the agent's
// read_screenshot tool — one or more JPEG tiles ordered top-to-bottom
// plus a flag that's set when the rendered document was too tall for
// the iframe's tile budget and trailing content was dropped. Tiles
// stay base64-encoded all the way through the broker so neither side
// pays a decode/re-encode round-trip — the MCP layer hands them
// straight to the agent's wire.
type CaptureResult struct {
	Tiles   []string
	Clipped bool
}

type screenshotResolution struct {
	tiles   []string
	clipped bool
	err     error
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
// responds, the session ends, or ctx is cancelled. Returns the ordered
// list of JPEG tiles the frontend captured (top-to-bottom) and a
// clipped flag set when the rendered document overran the tile budget.
func (b *ScreenshotBroker) Capture(ctx context.Context, threadID string) (CaptureResult, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return CaptureResult{}, fmt.Errorf("design: screenshot thread id required")
	}
	if b == nil || b.emit == nil {
		return CaptureResult{}, fmt.Errorf("design: screenshot broker unavailable")
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
		return CaptureResult{}, ctx.Err()
	case <-timer.C:
		b.cancel(requestID, fmt.Errorf("design: screenshot timed out after %s", CaptureMaxWait))
		return CaptureResult{}, fmt.Errorf("design: screenshot timed out after %s", CaptureMaxWait)
	case resolution := <-resultCh:
		if resolution.err != nil {
			return CaptureResult{}, resolution.err
		}
		return CaptureResult{Tiles: resolution.tiles, Clipped: resolution.clipped}, nil
	}
}

// Resolve completes a pending screenshot request. requestID is the
// opaque uuid the broker minted when Capture was called; thread
// validation is unnecessary because the id is unique across all
// in-flight requests for the lifetime of the broker. Tiles are taken
// top-to-bottom and stay base64-encoded — the MCP wire forwards
// them as-is. clipped indicates the page exceeded the frontend's
// tile budget and trailing tiles were dropped.
//
// validateScreenshotTiles caps count, per-tile encoded length, and
// aggregate encoded length. A request that fails validation does
// not leak the pending entry — it's released via Fail so the
// agent's blocking tool call surfaces a clean error instead of
// timing out at CaptureMaxWait.
func (b *ScreenshotBroker) Resolve(requestID string, tiles []string, clipped bool) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("design: screenshot requestID required")
	}
	if err := validateScreenshotTiles(tiles); err != nil {
		// Surface validation failures as a Fail so the parked
		// Capture goroutine returns rather than waiting on
		// CaptureMaxWait.
		_ = b.Fail(requestID, err.Error())
		return err
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
	case pending.resultCh <- screenshotResolution{tiles: tiles, clipped: clipped}:
	default:
	}
	return nil
}

// validateScreenshotTiles enforces the broker's caps. Mirrors what
// IngestScreenshot enforces at the wire boundary so callers that
// reach the broker through any path (tests, future bindings) get
// the same contract.
func validateScreenshotTiles(tiles []string) error {
	if len(tiles) == 0 {
		return fmt.Errorf("design: screenshot has no tiles")
	}
	if len(tiles) > MaxScreenshotTiles {
		return fmt.Errorf("design: screenshot tile count %d exceeds cap %d", len(tiles), MaxScreenshotTiles)
	}
	total := 0
	for i, encoded := range tiles {
		if encoded == "" {
			return fmt.Errorf("design: screenshot tile %d is empty", i)
		}
		if len(encoded) > MaxScreenshotTileBase64Bytes {
			return fmt.Errorf("design: screenshot tile %d size %d exceeds cap %d",
				i, len(encoded), MaxScreenshotTileBase64Bytes)
		}
		total += len(encoded)
		if total > MaxScreenshotTotalBase64Bytes {
			return fmt.Errorf("design: screenshot total size exceeds cap %d", MaxScreenshotTotalBase64Bytes)
		}
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
