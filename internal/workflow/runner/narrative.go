package runner

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Narrative recovery. A phase attempt's narrative is the one human-readable
// account of what the work did, and the whole system points at it: the wake
// carries its path, the triage seed inlines it, a person opening a run reads it.
//
// An agent phase is told to write it, but a `read-only` phase runs in a provider
// session that denies every file write (D22, spec §9), so the instruction is one
// it cannot follow. The runner therefore synthesizes the file from the session's
// final assistant text, which for such a phase IS the narrative it produced. The
// synthesized file is always marked as recovered: a human must be able to tell
// an account the agent wrote as a file from one the system lifted out of a
// message.

// RecoveredNarrativeHeader is the first line of every synthesized narrative.
const RecoveredNarrativeHeader = "_Recovered from the session's final message; no narrative file was written._"

// RecoverNarrative renders the narrative content for a turn that produced no
// narrative file of its own. `texts` are the turn's assistant texts oldest
// first, and `envelope` is the control envelope it ended with.
//
// The envelope is passed because a provider whose structured output IS its final
// message (Codex) would otherwise have that JSON recovered as the narrative. A
// text that decodes to the same JSON document as the envelope is that message,
// not prose, and is skipped.
//
// Reports false when the session produced no prose at all: absence stays
// absence rather than becoming a file that says nothing.
func RecoverNarrative(texts []string, envelope json.RawMessage) (string, bool) {
	for index := len(texts) - 1; index >= 0; index-- {
		text := strings.TrimSpace(texts[index])
		if text == "" || isJSONDocument(text, envelope) {
			continue
		}
		return RecoveredNarrativeHeader + "\n\n" + text + "\n", true
	}
	return "", false
}

// isJSONDocument reports whether text is the envelope itself rather than prose.
// Both sides go through the same decode/re-encode pass, so neither formatting
// nor key order can turn a match into a miss.
func isJSONDocument(text string, envelope json.RawMessage) bool {
	if len(envelope) == 0 {
		return false
	}
	candidate, err := canonicalJSON([]byte(text))
	if err != nil {
		return false
	}
	control, err := canonicalJSON(envelope)
	if err != nil {
		return false
	}
	return bytes.Equal(candidate, control)
}

func canonicalJSON(payload []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
