package claudetui

import (
	"fmt"
	"sort"
	"strings"
)

// image_placement.go is the read side of the composer's inline-image placement.
// The composer (frontend) lets a user drop an image mid-message; it records the
// drop point by embedding an "[Image #N]" marker in the message text at that
// offset (frontend/src/lib/utils/imagePlaceholders.ts). Send turns those markers
// back into positioned image pastes so claude-tui ingests each image where the
// user put it — see buildSendSteps. Claude's own paste handler then stamps its
// conversation-global "[Image #N]" label at that spot, so the inline position and
// the label are both correct; AO's per-message marker text is dropped at the split.

// imagePart is one ordered segment of a composed message: a run of text
// (imageIndex < 0) or an image to paste at this point (imageIndex into the send's
// ordered image paths).
type imagePart struct {
	text       string
	imageIndex int
}

// imagePlaceholderLabel formats the inline marker the composer embeds at an
// image's drop point: "[Image #N]", N 1-based in attachment order.
//
// This is a CROSS-LANGUAGE CONTRACT with the frontend's imagePlaceholderLabel
// (frontend/src/lib/utils/imagePlaceholders.ts — IMAGE_PLACEHOLDER_PREFIX +
// index + IMAGE_PLACEHOLDER_SUFFIX). ensureImagePlaceholders runs at send, so
// every attachment reaches Send with exactly one marker in `content`.
// TestImagePlaceholderLabelMatchesFrontend pins the format so a frontend change
// can't silently desync the split.
func imagePlaceholderLabel(oneBasedIndex int) string {
	return fmt.Sprintf("[Image #%d]", oneBasedIndex)
}

// splitContentByImageMarkers walks `content` against the send's `imageCount`
// ordered images and returns the message as ordered parts, each image positioned
// at its "[Image #i]" marker (the marker text itself is dropped).
//
// Matching mirrors the frontend's findImagePlaceholderRanges: for i = 1..N claim
// the FIRST not-yet-used occurrence of "[Image #i]", so a literal "[Image #1]" the
// user typed as prose doesn't also get treated as attachment 1's marker once 1 is
// placed. An image whose marker is absent (e.g. the user deleted the marker but
// kept the attachment) is appended after the text, so a turn never silently drops
// an image — defensive, since ensureImagePlaceholders should guarantee a marker.
func splitContentByImageMarkers(content string, imageCount int) []imagePart {
	if imageCount <= 0 {
		if content == "" {
			return nil
		}
		return []imagePart{{text: content, imageIndex: -1}}
	}

	type marker struct{ start, end, imageIndex int }
	markers := make([]marker, 0, imageCount)
	claimed := make([]bool, imageCount)
	usedStarts := make(map[int]bool, imageCount)
	for i := 0; i < imageCount; i++ {
		label := imagePlaceholderLabel(i + 1)
		start := firstUnusedIndex(content, label, usedStarts)
		if start < 0 {
			continue
		}
		usedStarts[start] = true
		claimed[i] = true
		markers = append(markers, marker{start: start, end: start + len(label), imageIndex: i})
	}
	sort.Slice(markers, func(a, b int) bool { return markers[a].start < markers[b].start })

	parts := make([]imagePart, 0, len(markers)*2+1)
	cursor := 0
	for _, m := range markers {
		if m.start > cursor {
			parts = append(parts, imagePart{text: content[cursor:m.start], imageIndex: -1})
		}
		parts = append(parts, imagePart{imageIndex: m.imageIndex})
		cursor = m.end
	}
	if cursor < len(content) {
		parts = append(parts, imagePart{text: content[cursor:], imageIndex: -1})
	}
	for i := 0; i < imageCount; i++ {
		if !claimed[i] {
			parts = append(parts, imagePart{imageIndex: i})
		}
	}
	return parts
}

// firstUnusedIndex returns the first byte offset of label in content that is not
// already in usedStarts, or -1. Mirrors the frontend's findUnusedLabelStart so
// both sides claim the same occurrence when a label repeats.
func firstUnusedIndex(content, label string, usedStarts map[int]bool) int {
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
