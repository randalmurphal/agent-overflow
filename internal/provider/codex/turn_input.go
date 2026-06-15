package codex

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// buildTurnInput shapes the user content + attachments into the input array Codex's
// turn/start and turn/steer both accept, placing each image where the user dropped
// it in the composer (its "[Image #N]" marker) instead of front-loading every image
// at the start of the turn. provider.SplitContentByImageMarkers drops the marker
// text and returns ordered text/image parts; Codex preserves input-item order and
// wraps each image in its OWN native, per-turn "<image name=[Image #N]>…</image>"
// tag, so AO emits no tags and only has to position the items.
//
// Images are sent as `localImage` (on-disk path), not a base64 `image` data URL:
// only the path variant gets Codex's NUMBERED <image name=…> tag (the data URL is
// wrapped in a plain, unnumbered <image>), and Codex reads + resizes the file
// itself, so the JSON-RPC payload stays small. The app layer resolves each
// attachment to a Path for Codex (resolveSendMessageAttachments routes Codex through
// the same path-only branch as claude-tui); a missing path is a wiring bug we fail
// loudly on rather than dropping the image. Empty content with no attachments is
// rejected here so neither caller has to branch on it.
func buildTurnInput(content string, attachments []provider.ImageAttachment) ([]map[string]any, error) {
	// Reject an empty turn up front (whitespace-only text and no images), so a
	// no-op steer fails fast instead of hitting the wire with a blank input vec.
	// Non-blank content keeps its inter-marker whitespace runs below — this guard
	// only fires when there is nothing to send at all.
	if strings.TrimSpace(content) == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("codex: turn input requires text or image")
	}
	parts := provider.SplitContentByImageMarkers(content, len(attachments))
	input := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		if part.ImageIndex >= 0 {
			attachment := attachments[part.ImageIndex]
			if attachment.Path == "" {
				return nil, fmt.Errorf("codex: image attachment %q has no on-disk path", attachment.ID)
			}
			input = append(input, map[string]any{
				"type": "localImage",
				"path": attachment.Path,
			})
			continue
		}
		input = append(input, map[string]any{
			"type":          "text",
			"text":          part.Text,
			"text_elements": []any{},
		})
	}
	return input, nil
}
