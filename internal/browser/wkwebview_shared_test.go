package browser

import (
	"regexp"
	"strings"
	"testing"
)

// The WKWebView engine's testable half is everything pure. These two rules are
// the ones a macOS-only mistake would be silent about: an identifier WebKit
// rejects costs a workspace its isolation without an error anywhere, and a
// full-document capture size that ignores its bounds turns one screenshot into
// an unbounded allocation.

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestWKStoreIdentifierIsAStableRFC4122UUID(t *testing.T) {
	id := wkStoreIdentifier("/Users/someone/code/agent-overflow")
	if !uuidPattern.MatchString(id) {
		t.Fatalf("identifier %q is not a version-4 UUID; WebKit would refuse it and the workspace would silently lose its persistent store", id)
	}
	if again := wkStoreIdentifier("/Users/someone/code/agent-overflow"); again != id {
		t.Fatal("the identifier must be stable: a workspace that hashes differently on the next boot loses its site data")
	}
	if other := wkStoreIdentifier("/Users/someone/code/other"); other == id {
		t.Fatal("two workspaces must not share one store")
	}
	if strings.Contains(id, "someone") {
		t.Fatal("the identifier must be a digest, never the workspace path")
	}
	if wkStoreIdentifier("") == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("the all-zero UUID is documented as invalid")
	}
}

func TestClampDocumentCaptureStaysWithinTheScreenshotBounds(t *testing.T) {
	// A page reporting a huge scroll extent must not become a huge allocation:
	// the frame is cropped to the same maximum afterwards regardless.
	got := clampDocumentCapture(99_000, 400_000, 1280, 800)
	if got.width != maxFullScreenshotWidth || got.height != maxFullScreenshotHeight {
		t.Fatalf("clamped = %+v, want the screenshot maximum", got)
	}
}

func TestClampDocumentCaptureNeverShrinksTheView(t *testing.T) {
	// Shrinking to capture would reflow the very page being captured.
	got := clampDocumentCapture(300, 200, 1280, 800)
	if got.width != 1280 || got.height != 800 {
		t.Fatalf("clamped = %+v, want the view's own size", got)
	}
}

func TestClampDocumentCaptureGrowsToATallDocument(t *testing.T) {
	got := clampDocumentCapture(1280, 5_000, 1280, 800)
	if got.width != 1280 || got.height != 5_000 {
		t.Fatalf("clamped = %+v, want the document's own height", got)
	}
}

func TestDocumentSizeScriptReadsBothScrollExtents(t *testing.T) {
	// body and documentElement disagree depending on the page's box model, so a
	// capture that reads only one of them truncates real documents.
	for _, want := range []string{"documentElement", "document.body", "scrollHeight", "scrollWidth"} {
		if !strings.Contains(wkDocumentSizeScript, want) {
			t.Fatalf("document size script must mention %q", want)
		}
	}
}
