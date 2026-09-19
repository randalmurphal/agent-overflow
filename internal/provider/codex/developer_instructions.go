package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// Developer instructions are the app-appended half of what the model reads
// before its own system prompt. They ride `developerInstructions` on
// `thread/start`, `thread/resume` and `thread/fork`, and nowhere else:
// `turn/start` has no such field, and the `developer_instructions: nil`
// AO sends in every turn's collaboration-mode settings is a different
// value entirely.
//
// Verified against codex-rs 0.153.4, which matches the installed CLI:
// the turn's instructions come from `TurnContext.developer_instructions`,
// cloned from the thread's session configuration
// (core/src/session/turn_context.rs), while collaboration mode carries
// its own `settings.developer_instructions` into a separate world-state
// section (core/src/context/world_state/collaboration_mode.rs). The
// per-turn override struct has no developer-instructions field at all, so
// a null in collaboration-mode settings cannot reach the thread value; it
// means "use the built-in instructions for the selected mode"
// (app-server-protocol v2/turn.rs, app-server turn_processor.rs
// normalize_collaboration_mode).
//
// Codex resolves developer instructions from CONFIG on a cold start, with
// no history fallback of the kind base instructions have, so an override
// that named only AO's guide would silently drop the user's own value.
// AO therefore reads the thread cwd's configured value first and appends
// to it, and sends nothing at all when it cannot.

// resolveDeveloperInstructions composes what this thread will carry: the
// cwd's own configured instructions with AO's guide appended. An empty
// guide, or a config read that failed, yields "" and the caller omits the
// override entirely rather than replacing a value it could not read.
func (s *Session) resolveDeveloperInstructions(ctx context.Context, guide string) string {
	guide = strings.TrimSpace(guide)
	if guide == "" {
		return ""
	}
	configured, err := s.configuredDeveloperInstructions(ctx)
	if err != nil {
		log.Printf("codex: read developer instructions for %s: %v", s.workDir, err)
		return ""
	}
	if configured == "" {
		return guide
	}
	return configured + "\n\n" + guide
}

// configuredDeveloperInstructions reads the cwd-scoped
// `developer_instructions` the way effectiveReviewModel reads
// `review_model`. The response's config object is snake_case.
func (s *Session) configuredDeveloperInstructions(ctx context.Context) (string, error) {
	raw, err := s.sendRequest(ctx, "config/read", map[string]any{
		"cwd":           s.workDir,
		"includeLayers": false,
	})
	if err != nil {
		return "", fmt.Errorf("codex: config/read for developer instructions: %w", err)
	}
	var response struct {
		Config struct {
			DeveloperInstructions string `json:"developer_instructions"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("codex: decode config/read for developer instructions: %w", err)
	}
	return strings.TrimSpace(response.Config.DeveloperInstructions), nil
}

func (s *Session) setDeveloperInstructions(text string) {
	s.mu.Lock()
	s.developerInstructions = text
	s.mu.Unlock()
}

// developerInstructionsValue is what a fork must carry forward. A fork is
// a new thread and resolves developer instructions from config like any
// cold start, so without this the child would lose the composed value the
// parent thread was started with.
func (s *Session) developerInstructionsValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.developerInstructions
}
