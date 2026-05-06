package design

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/screenshot"
)

// TestMaxScreenshotTilesMatchesSlicer pins the design-package
// MaxScreenshotTiles ceiling against the screenshot package's default
// slicer ceiling. The reactor passes MaxScreenshotTiles explicitly into
// SliceOptions so the two would not silently drift even if this test
// disappeared, but a tripping assertion gives the next author a clearer
// failure than a layout regression in a sample capture.
func TestMaxScreenshotTilesMatchesSlicer(t *testing.T) {
	if MaxScreenshotTiles != screenshot.DefaultMaxTiles {
		t.Fatalf("MaxScreenshotTiles=%d, screenshot.DefaultMaxTiles=%d — keep them aligned or remove the explicit pass in reactor.go",
			MaxScreenshotTiles, screenshot.DefaultMaxTiles)
	}
}

// TestReactorCaptureScreenshotWithoutCapturer pins the boot-order
// contract: a Reactor wired with a nil Capturer (early boot, or a test
// that doesn't exercise screenshots) returns a clear error rather than
// nil-deref'ing on tool dispatch. The MCP layer surfaces this via the
// `isError: true` tool-result envelope.
func TestReactorCaptureScreenshotWithoutCapturer(t *testing.T) {
	reactor := NewReactor(NewDiagnosticBuffer(nil), nil)

	_, err := reactor.CaptureScreenshot(context.Background(), "thread-x")
	if err == nil {
		t.Fatal("CaptureScreenshot with nil Capturer = nil err, want error")
	}
	if !strings.Contains(err.Error(), "screenshot capture unavailable") {
		t.Errorf("err = %v, want it to mention screenshot capture unavailable", err)
	}
}

// TestReactorCaptureScreenshotNilReactor protects the nil-receiver
// path the MCP layer's defensive checks rely on; the reactor is wired
// in app boot so a nil here means a programming error, but failing
// loudly beats a panic in production.
func TestReactorCaptureScreenshotNilReactor(t *testing.T) {
	var reactor *Reactor

	_, err := reactor.CaptureScreenshot(context.Background(), "thread-x")
	if err == nil {
		t.Fatal("CaptureScreenshot on nil Reactor = nil err, want error")
	}
}
