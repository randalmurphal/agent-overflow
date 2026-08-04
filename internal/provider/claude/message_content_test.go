package claude

import (
	"encoding/base64"
	"testing"

	"agent-overflow/internal/provider"
)

// TestBuildUserMessageBlocksInlinePlacement is the regression guard for the
// front-loading bug: images must land at their "[Image #N]" marker, not bunched at
// the start of the turn, and the marker text must be dropped. With the old build
// (one text block of the full content, then every image appended) this fails on the
// block ordering and on the marker text surviving in the text.
func TestBuildUserMessageBlocksInlinePlacement(t *testing.T) {
	att := func(id string) provider.ImageAttachment {
		return provider.ImageAttachment{ID: id, MimeType: "image/png", Data: []byte("png-" + id)}
	}

	content := "first [Image #1] then [Image #2] end"
	blocks, err := buildUserMessageBlocks(content, []provider.ImageAttachment{att("a"), att("b")}, false)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}

	wantTypes := []string{"text", "image", "text", "image", "text"}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("got %d blocks %+v, want %d", len(blocks), blocks, len(wantTypes))
	}
	for i, wt := range wantTypes {
		if blocks[i]["type"] != wt {
			t.Fatalf("block %d type = %v, want %s (blocks=%+v)", i, blocks[i]["type"], wt, blocks)
		}
	}
	// Text runs keep the spacing around each marker and DROP the marker itself.
	if blocks[0]["text"] != "first " || blocks[2]["text"] != " then " || blocks[4]["text"] != " end" {
		t.Fatalf("text runs not split at markers / marker text survived: %+v", blocks)
	}
	// Images are bound to attachments positionally (marker #i → attachment i), base64
	// inlined with their media type.
	src0 := blocks[1]["source"].(map[string]any)
	if src0["media_type"] != "image/png" || src0["data"] != base64.StdEncoding.EncodeToString([]byte("png-a")) {
		t.Fatalf("image at marker #1 bound wrong / not base64-inlined: %+v", src0)
	}
	src1 := blocks[3]["source"].(map[string]any)
	if src1["data"] != base64.StdEncoding.EncodeToString([]byte("png-b")) {
		t.Fatalf("image at marker #2 bound to wrong attachment: %+v", src1)
	}
}

// TestBuildUserMessageBlocksEdgeCases pins the boundary shapes: image-only, plain
// text, and the empty-turn rejection.
func TestBuildUserMessageBlocksEdgeCases(t *testing.T) {
	// Image-only (composer sent only a marker, which the split drops) → one image.
	blocks, err := buildUserMessageBlocks("[Image #1]", []provider.ImageAttachment{{ID: "a", MimeType: "image/png", Data: []byte("x")}}, false)
	if err != nil {
		t.Fatalf("image-only: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "image" {
		t.Fatalf("image-only blocks = %+v, want one image block", blocks)
	}

	// Text-only (no attachments) → one text block carrying the content verbatim.
	blocks, err = buildUserMessageBlocks("hello world", nil, false)
	if err != nil {
		t.Fatalf("text-only: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "hello world" {
		t.Fatalf("text-only blocks = %+v, want one text block", blocks)
	}

	// No text and no images is an empty turn — rejected, matching claude-tui.
	if _, err := buildUserMessageBlocks("", nil, false); err == nil {
		t.Fatal("expected error for an empty turn (no text, no images)")
	}

	// Whitespace-only with no images is also empty — rejected (parity with claude-tui
	// and Codex, both of which reject whitespace-only sends).
	if _, err := buildUserMessageBlocks("   ", nil, false); err == nil {
		t.Fatal("expected error for a whitespace-only turn (no images)")
	}

	// Whitespace around an image is NOT empty: the image makes it a real turn and the
	// surrounding whitespace run is preserved as its own text block.
	blocks, err = buildUserMessageBlocks("  ", []provider.ImageAttachment{{ID: "a", MimeType: "image/png", Data: []byte("x")}}, false)
	if err != nil {
		t.Fatalf("whitespace + image: %v", err)
	}
	if len(blocks) != 2 || blocks[0]["type"] != "text" || blocks[0]["text"] != "  " || blocks[1]["type"] != "image" {
		t.Fatalf(`whitespace + image blocks = %+v, want [text("  "), image]`, blocks)
	}
}
