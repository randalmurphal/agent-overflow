package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/workflow/def"
)

func promptGuidanceForEntry(item *runtimeItem) []GuidanceEntry {
	if item.entry == entryContinuation {
		return nil
	}
	return item.guidance
}

func phaseTurnLaunch(entry phaseEntry, threadID string, finalizeTakeover bool) (TurnLaunch, error) {
	if finalizeTakeover {
		if entry != entryContinuation {
			return TurnLaunch{}, fmt.Errorf("workflow phase launch: takeover finalize requires continuation entry")
		}
		return FinalizeThread(threadID)
	}
	switch entry {
	case entryFresh:
		if strings.TrimSpace(threadID) == "" {
			return FreshTurn(), nil
		}
		return ReuseThread(threadID)
	case entryContinuation:
		return ContinueThread(threadID)
	case entryRestart:
		if strings.TrimSpace(threadID) != "" {
			return TurnLaunch{}, fmt.Errorf("workflow phase launch: reconstructed entry cannot reuse thread %q", threadID)
		}
		return FreshTurn(), nil
	default:
		return TurnLaunch{}, fmt.Errorf("workflow phase launch: unknown entry kind %d", entry)
	}
}

// continuationFeedback carries only variables that changed since the parked
// attempt. A seed amendment must reach the resumed turn, but replaying the
// whole rendered prompt to accomplish that would defeat continuation mode.
func continuationFeedback(declarations map[string]def.Variable, vars, previous map[string]any, feedback *Feedback) *Feedback {
	if previous == nil {
		return cloneFeedback(feedback)
	}
	result := cloneFeedback(feedback)
	// Seeds are the only mutable inputs while a run is parked. Derived values
	// such as budget and history are recomputed on every variable-context build;
	// including them would turn an intentionally small resume message back into
	// a partial replay of the full prompt.
	for name := range declarations {
		value, exists := vars[name]
		if !exists {
			continue
		}
		old, existed := previous[name]
		if existed && sameJSONValue(old, value) {
			continue
		}
		if result == nil {
			result = &Feedback{}
		}
		if result.Values == nil {
			result.Values = make(map[string]any)
		}
		result.Values[name] = value
	}
	return result
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

const continuationAvailableNote = "continue from where the previous turn stopped"

const continuationUnavailableNote = "the previous attempt's provider session is unavailable, so reconstruct the same round from its full prompt and inputs"

func continuationUnavailableFeedback(feedback *Feedback) *Feedback {
	result := cloneFeedback(feedback)
	if result != nil && strings.Contains(result.Note, continuationAvailableNote) {
		result.Note = strings.Replace(result.Note, continuationAvailableNote, continuationUnavailableNote, 1)
		return result
	}
	return appendFeedbackNote(result, continuationUnavailableNote)
}

func cloneFeedback(feedback *Feedback) *Feedback {
	if feedback == nil {
		return nil
	}
	copy := &Feedback{Note: feedback.Note, Values: make(map[string]any, len(feedback.Values))}
	for name, value := range feedback.Values {
		copy.Values[name] = value
	}
	return copy
}

// parkStartFailure maps a runner startup failure onto a typed park reason by
// sentinel, never by string matching.
func (e *Engine) parkStartFailure(item *runtimeItem, cause error) error {
	reason := ReasonAgentError
	switch {
	case errors.Is(cause, ErrSetupFailed):
		reason = ReasonSetupFailed
	case errors.Is(cause, ErrWiringFailed):
		reason = ReasonWiringError
	}
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}
