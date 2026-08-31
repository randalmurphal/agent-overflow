package browser

import (
	"regexp"
	"strings"
	"testing"
)

// The WKWebView engine's testable half is everything pure. These rules are the
// ones a macOS-only mistake would be silent about: an identifier WebKit rejects
// costs a workspace its isolation without an error anywhere, a full-document
// capture size that ignores its bounds turns one screenshot into an unbounded
// allocation, and a site-data clear that reports the wrong outcome tells the
// user their cookies are gone when they are not, or the reverse.

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

// Nothing removed and nothing TO remove are both success. The second is the
// macOS 11-13 answer and the answer of any Mac that never persisted site data,
// and reporting it as a failure would tell the user the button did not work.
func TestClearSiteDataFailureTreatsNothingRemovedAsSuccess(t *testing.T) {
	for _, reported := range []string{"", "\n", "  \n \n"} {
		if err := wkClearSiteDataFailure(reported); err != nil {
			t.Fatalf("wkClearSiteDataFailure(%q) = %v, want nil: zero identifiers is a cleared engine", reported, err)
		}
	}
}

func TestClearSiteDataFailureNamesTheReason(t *testing.T) {
	err := wkClearSiteDataFailure("the file is locked\n")
	if err == nil {
		t.Fatal("a reported removal failure must not be swallowed: the user would be told their site data is gone")
	}
	if !strings.Contains(err.Error(), "the file is locked") {
		t.Fatalf("error %q must carry WebKit's own reason", err)
	}
}

// WebKit answers once per store, so an unwritable container reports the same
// sentence once per workspace the user has ever opened. One line, not a
// transcript — and never a count that hides how many stores actually failed.
func TestClearSiteDataFailureFoldsRepeatedAndExcessReasons(t *testing.T) {
	repeated := wkClearSiteDataFailure("same reason\nsame reason\nsame reason\nsame reason")
	if repeated == nil {
		t.Fatal("repeated failures are still failures")
	}
	if got := strings.Count(repeated.Error(), "same reason"); got != 1 {
		t.Fatalf("error %q repeats one reason %d times", repeated, got)
	}
	many := wkClearSiteDataFailure("one\ntwo\nthree\nfour\nfive")
	if many == nil {
		t.Fatal("five failures are still failures")
	}
	if !strings.Contains(many.Error(), "and 2 more") {
		t.Fatalf("error %q must account for the reasons it did not name", many)
	}
	if strings.Contains(many.Error(), "five") {
		t.Fatalf("error %q names more than %d reasons", many, wkClearFailureLimit)
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
