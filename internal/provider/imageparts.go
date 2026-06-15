package provider

import (
	"fmt"
	"sort"
	"strings"
)

// imageparts.go is the provider-agnostic read side of the composer's inline-image
// placement. The composer (frontend) lets a user drop an image mid-message; it
// records the drop point by embedding an "[Image #N]" marker in the message text
// at that offset (frontend/src/lib/utils/imagePlaceholders.ts). Every provider's
// Send turns those markers back into positioned images so the agent ingests each
// image where the user put it instead of front-loaded at the start of the turn —
// claude-tui pastes the file path at that spot, headless Claude emits an ordered
// image content block, and Codex emits a `localImage` input item. The split logic
// is identical across all three, so it lives here once.

// ContentPart is one ordered segment of a composed user message: a run of text
// (ImageIndex < 0) or an image to place at this point (ImageIndex into the send's
// ordered attachments). Each provider renders the parts into its own wire shape.
type ContentPart struct {
	Text       string
	ImageIndex int
}

// ImagePlaceholderLabel formats the inline marker the composer embeds at an
// image's drop point: "[Image #N]", N 1-based in attachment order.
//
// This is a CROSS-LANGUAGE CONTRACT with the frontend's imagePlaceholderLabel
// (frontend/src/lib/utils/imagePlaceholders.ts — IMAGE_PLACEHOLDER_PREFIX +
// index + IMAGE_PLACEHOLDER_SUFFIX). ensureImagePlaceholders runs at send, so
// every attachment reaches a provider's Send with exactly one marker in `content`.
// TestImagePlaceholderLabelMatchesFrontend pins the format so a frontend change
// can't silently desync the split.
func ImagePlaceholderLabel(oneBasedIndex int) string {
	return fmt.Sprintf("[Image #%d]", oneBasedIndex)
}

// SplitContentByImageMarkers walks `content` against the send's `imageCount`
// ordered images and returns the message as ordered parts, each image positioned
// at its "[Image #i]" marker (the marker text itself is dropped). The returned
// text parts are never empty (a part is emitted only for a non-empty span), so a
// caller can render every text part without an emptiness guard.
//
// Matching mirrors the frontend's findImagePlaceholderRanges: for i = 1..N claim
// the FIRST not-yet-used occurrence of "[Image #i]", so a literal "[Image #1]" the
// user typed as prose doesn't also get treated as attachment 1's marker once 1 is
// placed. An image whose marker is absent (e.g. the user deleted the marker but
// kept the attachment) is appended after the text, so a turn never silently drops
// an image — defensive, since ensureImagePlaceholders should guarantee a marker.
//
// Numbering is per-message (1..N) today; the thread-global image counter will pass
// the attachments' thread-global label numbers instead, which is the only change
// this helper needs to support that — the positioning logic is identical.
func SplitContentByImageMarkers(content string, imageCount int) []ContentPart {
	if imageCount <= 0 {
		if content == "" {
			return nil
		}
		return []ContentPart{{Text: content, ImageIndex: -1}}
	}

	type marker struct{ start, end, imageIndex int }
	markers := make([]marker, 0, imageCount)
	claimed := make([]bool, imageCount)
	usedStarts := make(map[int]bool, imageCount)
	for i := 0; i < imageCount; i++ {
		label := ImagePlaceholderLabel(i + 1)
		start := firstUnusedMarkerIndex(content, label, usedStarts)
		if start < 0 {
			continue
		}
		usedStarts[start] = true
		claimed[i] = true
		markers = append(markers, marker{start: start, end: start + len(label), imageIndex: i})
	}
	sort.Slice(markers, func(a, b int) bool { return markers[a].start < markers[b].start })

	parts := make([]ContentPart, 0, len(markers)*2+1)
	cursor := 0
	for _, m := range markers {
		if m.start > cursor {
			parts = append(parts, ContentPart{Text: content[cursor:m.start], ImageIndex: -1})
		}
		parts = append(parts, ContentPart{ImageIndex: m.imageIndex})
		cursor = m.end
	}
	if cursor < len(content) {
		parts = append(parts, ContentPart{Text: content[cursor:], ImageIndex: -1})
	}
	for i := 0; i < imageCount; i++ {
		if !claimed[i] {
			parts = append(parts, ContentPart{ImageIndex: i})
		}
	}
	return parts
}

// firstUnusedMarkerIndex returns the first byte offset of label in content that is
// not already in usedStarts, or -1. Mirrors the frontend's findUnusedLabelStart so
// both sides claim the same occurrence when a label repeats.
func firstUnusedMarkerIndex(content, label string, usedStarts map[int]bool) int {
	from := 0
	for {
		rel := strings.Index(content[from:], label)
		if rel < 0 {
			return -1
		}
		abs := from + rel
		if !usedStarts[abs] {
			return abs
		}
		from = abs + len(label)
	}
}
