package claude

import (
	"encoding/base64"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// buildUserMessageBlocks shapes the user content + image attachments into the
// ordered Anthropic Messages content blocks Send writes to the CLI's stdin,
// placing each image where the user dropped it in the composer (its "[Image #N]"
// marker) instead of front-loading every image at the start of the turn.
//
// An empty turn (whitespace-only text and no images) is rejected up front, matching
// the claude-tui and Codex send guards, so a no-op send fails loudly instead of
// hitting the wire with a blank message. provider.SplitContentByImageMarkers then
// drops the marker text and returns the message as interleaved text/image parts;
// each text run becomes a text block and each image a base64 block. The Anthropic
// Messages API has no local-path image source, so headless Claude always inlines the
// bytes — unlike claude-tui (pastes the path) and Codex (sends a `localImage` path).
func buildUserMessageBlocks(content string, attachments []provider.ImageAttachment) ([]map[string]any, error) {
	if strings.TrimSpace(content) == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("claude: user message requires text or image content")
	}
	blocks := make([]map[string]any, 0, 1+2*len(attachments))
	for _, part := range provider.SplitContentByImageMarkers(content, len(attachments)) {
		if part.ImageIndex >= 0 {
			attachment := attachments[part.ImageIndex]
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": attachment.MimeType,
					"data":       base64.StdEncoding.EncodeToString(attachment.Data),
				},
			})
			continue
		}
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": part.Text,
		})
	}
	return blocks, nil
}
