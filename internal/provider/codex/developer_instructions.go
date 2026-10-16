package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
//
// A resumed thread does not show a changed value to the model. Codex
// restores the model's context from the rollout
// (record_initial_history -> apply_rollout_reconstruction), the restored
// TurnContextItem carries no developer instructions, and developer
// instructions are not a world-state section, so no diff is emitted: the
// model keeps the text the thread started with until a compaction rebuilds
// the context from the thread-level value. AO therefore tracks the digest
// of the text each thread last received and, when a resume composes a
// different one, injects it once as a developer message with
// `thread/inject_items`, which Codex writes to the rollout so later resumes
// keep it (docs/references/codex-wire.md, verified against 0.162.1).

// DeveloperInstructionsDelivery is the record of what a thread's model has
// read as developer instructions. A Config without one (a throwaway
// session that only forks or reverts) neither checks nor injects.
type DeveloperInstructionsDelivery struct {
	// Received is the DeveloperInstructionsDigest of the text the thread
	// being resumed last received, or "" when nothing is recorded. An
	// unknown record never matches, so the composed text is injected.
	Received string
	// Record persists that the Codex thread codexThreadID now holds the
	// text with this digest: after a fresh thread/start and after an
	// injection.
	Record func(codexThreadID, digest string) error
}

// DeveloperInstructionsDigest identifies one composed text.
func DeveloperInstructionsDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// composedDeveloperInstructions is one composition: text is what the model
// should read (the cwd's configured instructions, then AO's guide), and
// override the `developerInstructions` value to send, empty when AO has
// nothing to append so the configured value stands. known is false when
// the configured half could not be read.
type composedDeveloperInstructions struct {
	text     string
	override string
	known    bool
}

// composeDeveloperInstructions reads the cwd's configured instructions and
// appends guide. The read is skipped only when there is nothing to append
// and nothing to check. A failed read sends no override rather than
// replacing a value AO could not read, and reaches the thread as a
// non-fatal error.
func (s *Session) composeDeveloperInstructions(ctx context.Context, guide string) composedDeveloperInstructions {
	guide = strings.TrimSpace(guide)
	if guide == "" && s.developerInstructionsRecord == nil {
		return composedDeveloperInstructions{}
	}
	configured, err := s.configuredDeveloperInstructions(ctx)
	if err != nil {
		if guide != "" {
			s.emitNonFatalError(fmt.Sprintf(
				"Could not read this workspace's Codex developer instructions, so Agent Overflow's guide was left out of this session: %v", err))
		} else {
			s.emitNonFatalError(fmt.Sprintf(
				"Could not read this workspace's Codex developer instructions, so a change to them may not reach this thread: %v", err))
		}
		return composedDeveloperInstructions{}
	}
	composed := composedDeveloperInstructions{text: guide, known: true}
	switch {
	case configured == "":
	case guide == "":
		composed.text = configured
	default:
		composed.text = configured + "\n\n" + guide
	}
	if guide != "" {
		composed.override = composed.text
	}
	return composed
}

// EnsureDeveloperInstructions composes guide with the cwd's configured
// instructions and injects the result when the thread has not received
// it. Session start runs the same check (ensureDeveloperInstructions); a
// caller that applies a change before a turn calls this while the thread
// is idle, before turn/start. The thread-level value Codex installs after
// a compaction stays the one this session started or resumed with.
func (s *Session) EnsureDeveloperInstructions(ctx context.Context, guide string) error {
	return s.ensureDeveloperInstructions(ctx, s.composeDeveloperInstructions(ctx, guide))
}

// ensureDeveloperInstructions injects composed's text as one developer
// message when its digest differs from the one the thread last received,
// then records the new digest. An untracked session and an unknown
// composition do nothing. Either failure leaves the next session start to
// send the text again.
func (s *Session) ensureDeveloperInstructions(ctx context.Context, composed composedDeveloperInstructions) error {
	if s.developerInstructionsRecord == nil || !composed.known {
		return nil
	}
	digest := DeveloperInstructionsDigest(composed.text)
	s.mu.Lock()
	received := s.developerInstructionsReceived
	s.mu.Unlock()
	if digest == received {
		return nil
	}
	if _, err := s.sendRequest(ctx, "thread/inject_items", map[string]any{
		"threadId": s.rootThreadID(),
		"items": []any{map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": developerInstructionsUpdate(composed.text),
			}},
		}},
	}); err != nil {
		return fmt.Errorf("codex: inject changed developer instructions: %w", err)
	}
	return s.recordDeveloperInstructions(digest)
}

// recordDeveloperInstructions notes that the thread now holds the text
// with digest. A failed record costs one repeated injection at the next
// session start.
func (s *Session) recordDeveloperInstructions(digest string) error {
	s.mu.Lock()
	s.developerInstructionsReceived = digest
	s.mu.Unlock()
	if err := s.developerInstructionsRecord(s.rootThreadID(), digest); err != nil {
		return fmt.Errorf("codex: record delivered developer instructions: %w", err)
	}
	return nil
}

// forgetDeveloperInstructions drops what the session knows the thread
// received. A history cut can remove an injected message.
func (s *Session) forgetDeveloperInstructions() {
	s.mu.Lock()
	s.developerInstructionsReceived = ""
	s.mu.Unlock()
}

// reportDeveloperInstructionsFailure puts a failed check on the thread.
func (s *Session) reportDeveloperInstructionsFailure(err error) {
	if err == nil {
		return
	}
	s.emitNonFatalError(fmt.Sprintf(
		"Agent Overflow could not confirm that this Codex thread has its current developer instructions; its next session start sends them again: %v", err))
}

// developerInstructionsUpdate is the injected message. It names what it
// replaces narrowly: the permissions, environment and collaboration-mode
// messages are developer messages too, and stay in force.
func developerInstructionsUpdate(text string) string {
	if text == "" {
		return "The developer instructions given at the start of this conversation, and any later update of them, no longer apply. Other context, such as permissions and the environment, is unchanged."
	}
	return "The developer instructions for this conversation have changed. The text below replaces the developer instructions given at its start and any earlier update of them. Other context, such as permissions and the environment, is unchanged.\n\n" + text
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
