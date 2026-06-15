package codex

import (
	"testing"

	"agent-overflow/internal/provider"
)

// TestBuildTurnInputInlinePlacement is the regression guard for the front-loading
// bug on Codex: each image must land at its "[Image #N]" marker as a `localImage`
// path item (which earns Codex's native numbered <image name=…> tag), not be
// appended at the end as a base64 `image` data URL. With the old build this fails on
// the item ordering, the item type, and the surviving marker text.
func TestBuildTurnInputInlinePlacement(t *testing.T) {
	att := func(id, path string) provider.ImageAttachment {
		return provider.ImageAttachment{ID: id, MimeType: "image/png", Path: path}
	}

	content := "look at [Image #1] and [Image #2] done"
	input, err := buildTurnInput(content, []provider.ImageAttachment{att("a", "/p/a.png"), att("b", "/p/b.png")})
	if err != nil {
		t.Fatalf("buildTurnInput: %v", err)
	}

	wantTypes := []string{"text", "localImage", "text", "localImage", "text"}
	if len(input) != len(wantTypes) {
		t.Fatalf("got %d items %+v, want %d", len(input), input, len(wantTypes))
	}
	for i, wt := range wantTypes {
		if input[i]["type"] != wt {
			t.Fatalf("item %d type = %v, want %s (input=%+v)", i, input[i]["type"], wt, input)
		}
	}
	// Text runs keep the spacing around each marker and DROP the marker itself; the
	// text item shape Codex's v2 wire expects carries an (empty) text_elements array.
	if input[0]["text"] != "look at " || input[2]["text"] != " and " || input[4]["text"] != " done" {
		t.Fatalf("text runs not split at markers / marker text survived: %+v", input)
	}
	// text_elements must be a non-nil empty array so it marshals to "[]" (Codex's v2
	// Text item shape), not a nil that would marshal to "null".
	if te, ok := input[0]["text_elements"].([]any); !ok || te == nil {
		t.Fatalf("text item text_elements = %v, want empty []any{}: %+v", input[0]["text_elements"], input[0])
	}
	// Images are bound positionally (marker #i → attachment i) and sent as a path —
	// AO emits no <image name> tag (Codex core adds it). No base64 data URL.
	if input[1]["path"] != "/p/a.png" {
		t.Fatalf("image at marker #1 path = %v, want /p/a.png", input[1]["path"])
	}
	if input[3]["path"] != "/p/b.png" {
		t.Fatalf("image at marker #2 bound to wrong attachment: %v", input[3]["path"])
	}
	if _, hasURL := input[1]["url"]; hasURL {
		t.Fatalf("localImage item must not carry a data url: %+v", input[1])
	}
}

// TestBuildTurnInputErrors pins the loud-failure paths: a path-less attachment is a
// wiring bug (resolveSendMessageAttachments must route Codex through the path-only
// branch), and an empty turn carries nothing to send.
func TestBuildTurnInputErrors(t *testing.T) {
	if _, err := buildTurnInput("[Image #1]", []provider.ImageAttachment{{ID: "a", MimeType: "image/png"}}); err == nil {
		t.Fatal("expected error for an attachment with no on-disk path")
	}
	if _, err := buildTurnInput("", nil); err == nil {
		t.Fatal("expected error for an empty turn (no text, no images)")
	}
	// Whitespace-only with no images is also empty — rejected by the up-front guard.
	if _, err := buildTurnInput("   ", nil); err == nil {
		t.Fatal("expected error for a whitespace-only turn (no images)")
	}
}

// TestBuildTurnInputWhitespaceWithImage pins the boundary the empty-guard must NOT
// swallow: whitespace content WITH an image is a real turn — the up-front guard only
// fires when there are no attachments — and the whitespace run is kept as its own
// text item alongside the image.
func TestBuildTurnInputWhitespaceWithImage(t *testing.T) {
	input, err := buildTurnInput("  ", []provider.ImageAttachment{{ID: "a", MimeType: "image/png", Path: "/p/a.png"}})
	if err != nil {
		t.Fatalf("whitespace + image: %v", err)
	}
	if len(input) != 2 || input[0]["type"] != "text" || input[0]["text"] != "  " || input[1]["type"] != "localImage" {
		t.Fatalf(`whitespace + image input = %+v, want [text("  "), localImage]`, input)
	}
}
