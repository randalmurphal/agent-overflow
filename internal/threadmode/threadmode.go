// Package threadmode owns the pure validators and parsers behind the
// thread interaction-mode (chat/plan/design) and runtime-mode
// (approval-required / auto-accept-edits / full-access) axes.
//
// The persistence and session-restart orchestration that consume these
// validators stay in main package — this package only knows the legal
// values and how to parse them.
package threadmode

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// DefaultCreateMode is what an empty `mode` field in CreateThread
// normalises to. Picked so existing callers that don't care about
// interaction mode keep working.
const DefaultCreateMode = "chat"

// ManualSelectionModes is the set the UI / new-thread flow is allowed
// to set at creation time. "discussion" is intentionally excluded —
// those threads come through StartDiscussion because they require a
// deliberation channel and participant child threads. Letting the UI
// set "discussion" directly would produce orphan threads the discussion
// runtime never knows about.
var ManualSelectionModes = map[string]struct{}{
	"chat":   {},
	"plan":   {},
	"design": {},
}

// PostCreationModes is the set the UI is allowed to mutate into via
// the chat/plan agent-mode toggle. Thread *type* (design / discussion)
// is determined at creation and is immutable thereafter — switching
// the type of a live thread would orphan its associated runtime state
// (design artifacts, deliberation channel) and confuse the UI shell.
// Internal callers that need to flip a chat thread's interaction mode
// (sendMessage's plan→chat saga, proposed-plan revisions) only ever
// move between chat and plan.
var PostCreationModes = map[string]struct{}{
	"chat": {},
	"plan": {},
}

// ValidateCreate normalises the mode for CreateThread. An empty
// string is accepted and normalised to DefaultCreateMode so existing
// callers that don't care about the mode keep working. "discussion"
// is rejected to keep StartDiscussion as the only path that produces
// discussion-mode threads.
func ValidateCreate(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if trimmed == "" {
		return DefaultCreateMode, nil
	}
	if _, ok := ManualSelectionModes[trimmed]; !ok {
		return "", fmt.Errorf("invalid mode %q (allowed: chat, plan, design)", trimmed)
	}
	return trimmed, nil
}

// ValidateSet validates a mode for UpdateThreadMode. Only chat and
// plan are accepted: design and discussion are immutable thread types
// set at creation time. The frontend's agent-mode toggle (chat ↔
// plan) is the only caller that should hit UpdateThreadMode at user-
// facing scope; internal callsites (proposed-plan saga) only ever
// pass chat or plan.
func ValidateSet(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if _, ok := PostCreationModes[trimmed]; !ok {
		return "", fmt.Errorf("invalid mode %q (allowed: chat, plan)", trimmed)
	}
	return trimmed, nil
}

// IsPostCreationMode reports whether the given mode is one the UI is
// allowed to mutate into post-creation. Callers use this to validate
// that a *current* thread mode is still chat/plan before allowing the
// UpdateThreadMode flip — design and discussion threads are immutable.
func IsPostCreationMode(mode string) bool {
	_, ok := PostCreationModes[strings.TrimSpace(mode)]
	return ok
}

// ParseRuntime validates and normalises a runtime-mode string. Returns
// an error for unrecognised values; the empty string is rejected
// (callers that want optional behaviour should use ParseOptionalRuntime).
func ParseRuntime(mode string) (provider.RuntimeMode, error) {
	normalized := provider.RuntimeMode(strings.TrimSpace(mode))
	switch normalized {
	case provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid runtime mode %q", mode)
	}
}

// ParseOptionalRuntime parses an optional runtime-mode value. An empty
// input returns (zero, false, nil) so callers can branch on "no value
// supplied"; a non-empty input runs through ParseRuntime.
func ParseOptionalRuntime(mode string) (provider.RuntimeMode, bool, error) {
	if strings.TrimSpace(mode) == "" {
		return "", false, nil
	}
	normalized, err := ParseRuntime(mode)
	if err != nil {
		return "", false, err
	}
	return normalized, true, nil
}
