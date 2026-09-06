package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// App feature guidance augments the effective native developer instructions.
// Replacing that field outright would discard config.toml/profile instructions.
// Codex resolves this field from config on each cold start/resume (unlike base
// instructions, it is not inherited from session_meta). When the feature is
// absent we omit the override and leave the native configuration untouched.
func (s *Session) appendDeveloperInstructions(ctx context.Context, extra string) (string, error) {
	raw, err := s.sendRequest(ctx, "config/read", map[string]any{"cwd": s.workDir, "includeLayers": false})
	if err != nil {
		return "", fmt.Errorf("codex: read native instructions before adding app tools: %w", err)
	}
	var response struct {
		Config struct {
			DeveloperInstructions string `json:"developer_instructions"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("codex: read native developer instructions: %w", err)
	}
	if native := response.Config.DeveloperInstructions; strings.TrimSpace(native) != "" {
		return native + "\n\n" + extra, nil
	}
	return extra, nil
}
