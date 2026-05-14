package codex

import (
	"encoding/base64"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// buildTurnInput shapes the user content + attachments into the input array
// Codex's turn/start and turn/steer both accept. Empty content with no
// attachments is rejected here so neither caller has to branch on it.
func buildTurnInput(content string, attachments []provider.ImageAttachment) ([]map[string]any, error) {
	input := make([]map[string]any, 0, 1+len(attachments))
	if strings.TrimSpace(content) != "" {
		input = append(input, map[string]any{
			"type":          "text",
			"text":          content,
			"text_elements": []any{},
		})
	}
	for _, attachment := range attachments {
		encoded := base64.StdEncoding.EncodeToString(attachment.Data)
		input = append(input, map[string]any{
			"type": "image",
			"url":  "data:" + attachment.MimeType + ";base64," + encoded,
		})
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("codex: turn input requires text or image")
	}
	return input, nil
}
