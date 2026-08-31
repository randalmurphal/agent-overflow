package discussion

import (
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

	"github.com/google/uuid"
)

// ParticipantPlan is the derived per-participant blueprint produced by
// BuildParticipantPlans: the child Thread row that the App should
// CreateThread + the system prompt to install on its per-thread
// system-prompt map before the provider session starts. Application
// orchestration in internal/discussionapp consumes a slice of these
// to fan out CreateThread / startSession / channel-link / cleanup.
type ParticipantPlan struct {
	Thread       store.Thread
	SystemPrompt string
}

// BuildParticipantPlans turns a discussion definition into a slice of
// per-participant plans rooted at the supplied parent thread. The
// caller supplies the timestamp (in milliseconds) so tests can pin
// CreatedAt / UpdatedAt deterministically.
//
// A participant's provider / model fall back to the parent thread's
// when blank. Either being unresolved after that fallback is a hard
// error — a participant with no provider can't spawn a session, and
// silently dropping the row would mask a misconfigured definition.
func BuildParticipantPlans(parent store.Thread, def store.DiscussionDefinition, nowMillis int64) ([]ParticipantPlan, error) {
	plans := make([]ParticipantPlan, 0, len(def.Participants))
	for _, participant := range def.Participants {
		role := strings.TrimSpace(participant.Role)
		providerName := stringsx.FirstNonEmptyTrimmed(participant.Provider, parent.Provider)
		model := stringsx.FirstNonEmptyTrimmed(participant.Model, parent.Model)
		if providerName == "" {
			return nil, fmt.Errorf("discussion participant %q is missing a provider", role)
		}
		if model == "" {
			return nil, fmt.Errorf("discussion participant %q is missing a model", role)
		}

		child := store.Thread{
			ID:             uuid.NewString(),
			ProjectID:      parent.ProjectID,
			ProjectPath:    parent.ProjectPath,
			Title:          fmt.Sprintf("%s - %s", parent.Title, FormatRole(role)),
			Provider:       providerName,
			WorkspacePath:  parent.WorkspacePath,
			Model:          model,
			WorktreePath:   parent.WorktreePath,
			Branch:         parent.Branch,
			Mode:           "discussion",
			ParentThreadID: parent.ID,
			CreatedAt:      nowMillis,
			UpdatedAt:      nowMillis,
		}
		plans = append(plans, ParticipantPlan{
			Thread:       child,
			SystemPrompt: BuildParticipantPrompt(role, participant.System),
		})
	}
	return plans, nil
}

// discussionProtocolPreamble tells a participant how the turn-driving
// mechanics in internal/discussionapp present other speakers to it:
// every other participant's and the human's contributions arrive as
// plain user messages (see turnprompt.go), so the model needs to know
// not to narrate the protocol itself back into its reply.
const discussionProtocolPreamble = "You are one voice among several participants in a multi-participant deliberation. " +
	"Messages from the other participants and from the human moderating the discussion arrive as user messages, " +
	"each labeled with who sent it. Respond with your next contribution only — no meta commentary about the discussion protocol itself. " +
	"When you believe the discussion has fully run its course and you have nothing further to add, end your message with a final line " +
	"starting with \"CONCLUDE:\" followed by a one-sentence summary of the outcome. The discussion only ends early once every participant's " +
	"latest message carries a CONCLUDE line, so keep contributing normally until then."

// BuildParticipantPrompt joins the discussion-context preamble, the
// discussion-protocol paragraph, and the participant's raw system
// prompt. Blank segments are dropped and remaining segments are
// separated by a blank line so providers see a single continuous
// prompt.
func BuildParticipantPrompt(role, rawSystem string) string {
	return joinPrompts(
		fmt.Sprintf("You are the %s participant in a discussion thread.", FormatRole(role)),
		discussionProtocolPreamble,
		rawSystem,
	)
}

// RoleFromThreadTitle reverses BuildParticipantPlans's title
// formatting ("<parent title> - <FormattedRole>") to recover the
// formatted role string. The deliberation runtime uses it when
// composing the `<from>:<role>` prefix on assistant channel posts so
// the speaker label stays consistent with the child thread title.
//
// Returns "" for an empty / whitespace-only title; returns the entire
// trimmed title when no " - " separator is present (e.g. legacy or
// renamed threads).
func RoleFromThreadTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	idx := strings.LastIndex(title, " - ")
	if idx < 0 {
		return title
	}
	role := strings.TrimSpace(title[idx+3:])
	if role == "" {
		return title
	}
	return role
}

// FormatRole title-cases a discussion role identifier, splitting on
// `-` / `_` / space and rejoining with spaces. Empty / whitespace
// inputs render as "Participant" so the discussion thread title still
// reads cleanly.
func FormatRole(role string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(role), func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return "Participant"
	}
	return strings.Join(parts, " ")
}

func joinPrompts(parts ...string) string {
	joined := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			joined = append(joined, part)
		}
	}
	return strings.Join(joined, "\n\n")
}
