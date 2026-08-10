package def

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/workflow/memory"
)

// The envelope's campaign-memory channel. What a note IS lives in
// `internal/workflow/memory` — the kind vocabulary, the bounds, the validation
// — and this file is only the two things an envelope adds: the schema fragment
// a provider is held to, and the post-validation that catches what a schema
// cannot express.
//
// The two overlap on purpose. The schema is what both CLIs enforce before the
// turn, so a well-behaved provider never sends a bad kind at all; post-validation
// is what a hand-written tool envelope and every frozen snapshot are judged by,
// and it is the layer that refuses an author-supplied `provenance`.

// envelopeMemorySchema is the generated fragment for the `memory` control field.
// Strict mode has no optional, so the array is required-and-nullable exactly as
// `narrative` is, and every property of an entry is listed in `required` with
// the optional one widened to admit null (internal/providerschema).
func envelopeMemorySchema() map[string]any {
	return map[string]any{
		"type": []string{"array", "null"},
		"description": "Durable lessons for later work in this campaign. Null when there is nothing worth " +
			"recording. Provenance is stamped by the system; do not supply it.",
		"maxItems": MaxEnvelopeMemoryNotes,
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"files", "kind", "text"},
			"properties": map[string]any{
				"kind": map[string]any{
					"type": "string",
					"enum": memory.Kinds,
					"description": "pattern: a shape that worked. warning: a trap that looked right. " +
						"learning: a fact about the environment or codebase. handoff: state for the next element.",
				},
				"text": map[string]any{
					"type":        "string",
					"maxLength":   memory.MaxTextBytes,
					"description": "The lesson, written for a reader with no other context.",
				},
				"files": map[string]any{
					"type":     []string{"array", "null"},
					"maxItems": memory.MaxFiles,
					"items":    map[string]any{"type": "string"},
				},
			},
		},
	}
}

// MaxEnvelopeMemoryNotes bounds how many notes one envelope may carry. The
// per-tree total is deliberately unbounded — a campaign legitimately accrues
// hundreds — but one element's single turn producing more than this is an
// element writing a narrative in the wrong channel, and the bound is what makes
// that a validation finding it can correct rather than a log it floods.
const MaxEnvelopeMemoryNotes = 20

// validateEnvelopeMemory checks the `memory` field of an envelope and appends a
// finding per problem. Absent and null are both "nothing to record" — there is
// no branch rule here, exactly as there is none for `narrative`: an element that
// finished, one that has to ask, and one that got stuck all learned things.
func validateEnvelopeMemory(raw json.RawMessage, findings *[]EnvelopeFinding) {
	drafts, ok := decodeEnvelopeMemory(raw, findings)
	if !ok {
		return
	}
	if len(drafts) > MaxEnvelopeMemoryNotes {
		*findings = append(*findings, EnvelopeFinding{
			Path: "$." + EnvelopeMemoryField,
			Message: fmt.Sprintf("carries %d notes; at most %d may be recorded in one envelope",
				len(drafts), MaxEnvelopeMemoryNotes),
		})
	}
	for index, entry := range drafts {
		root := fmt.Sprintf("$.%s[%d]", EnvelopeMemoryField, index)
		for name := range entry.extra {
			// `provenance` and `at` are the names an element would reach for, and
			// both are the system's answer to "who wrote this and when". Refusing
			// them here is what makes a supplied provenance impossible rather than
			// merely ignored.
			*findings = append(*findings, EnvelopeFinding{
				Path:    root + "." + name,
				Message: "property is not allowed; provenance and timestamps are stamped by the system",
			})
		}
		for _, finding := range memory.ValidateDraft(entry.draft) {
			*findings = append(*findings, EnvelopeFinding{Path: root + finding.Path, Message: finding.Message})
		}
	}
}

// envelopeMemoryEntry is one decoded entry plus whatever names it carried that
// a draft has no field for. The extras are collected rather than discarded
// because encoding/json would silently drop them and a supplied provenance
// would then look accepted.
type envelopeMemoryEntry struct {
	draft memory.Draft
	extra map[string]struct{}
}

// decodeEnvelopeMemory reads the field, reporting a shape failure and answering
// false when there is nothing further to check. Absent, null, and an empty array
// are all "no notes" and answer true with nothing to iterate.
func decodeEnvelopeMemory(raw json.RawMessage, findings *[]EnvelopeFinding) ([]envelopeMemoryEntry, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		*findings = append(*findings, EnvelopeFinding{
			Path:    "$." + EnvelopeMemoryField,
			Message: "must be an array of {kind, text, files} objects or null",
		})
		return nil, false
	}
	decoded := make([]envelopeMemoryEntry, 0, len(entries))
	for index, entry := range entries {
		root := fmt.Sprintf("$.%s[%d]", EnvelopeMemoryField, index)
		converted := envelopeMemoryEntry{extra: map[string]struct{}{}}
		malformed := false
		for name, value := range entry {
			switch name {
			case "kind":
				if err := json.Unmarshal(value, &converted.draft.Kind); err != nil {
					*findings = append(*findings, EnvelopeFinding{Path: root + ".kind", Message: "must be a string"})
					malformed = true
				}
			case "text":
				if err := json.Unmarshal(value, &converted.draft.Text); err != nil {
					*findings = append(*findings, EnvelopeFinding{Path: root + ".text", Message: "must be a string"})
					malformed = true
				}
			case "files":
				if string(value) == "null" {
					continue
				}
				if err := json.Unmarshal(value, &converted.draft.Files); err != nil {
					*findings = append(*findings, EnvelopeFinding{
						Path: root + ".files", Message: "must be an array of strings or null"})
					malformed = true
				}
			default:
				converted.extra[name] = struct{}{}
			}
		}
		if malformed {
			// The entry's own shape is already reported; running the draft rules
			// over half-decoded values would bury that under derived findings.
			continue
		}
		decoded = append(decoded, converted)
	}
	return decoded, true
}

// SplitEnvelopeMemory separates the recorded notes from a control envelope,
// returning the drafts and the envelope with the field removed. It is the same
// seam `SplitEnvelopeNarrative` is, and for the same reason: gate evaluation, a
// join's `units` results, call synthesis, and the persisted attempt envelope all
// read the returned payload, and none of them has any business carrying prose.
//
// Anything that does not decode as a JSON object, and any envelope with no
// memory field, is returned byte-for-byte unchanged. A field that is present but
// malformed strips the same way and yields no drafts: post-validation has
// already reported it, and leaving it in place would hand the engine the slot
// this exists to keep empty.
func SplitEnvelopeMemory(payload json.RawMessage) ([]memory.Draft, json.RawMessage) {
	var envelope map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil {
		return nil, payload
	}
	raw, present := envelope[EnvelopeMemoryField]
	if !present {
		return nil, payload
	}
	delete(envelope, EnvelopeMemoryField)
	stripped, err := json.Marshal(envelope)
	if err != nil {
		return nil, payload
	}
	var drafts []memory.Draft
	if json.Unmarshal(raw, &drafts) != nil {
		return nil, stripped
	}
	kept := make([]memory.Draft, 0, len(drafts))
	for _, draft := range drafts {
		// The app appends what survives here without re-deciding validity, so an
		// entry post-validation would have refused must not reach the log. A
		// well-formed envelope has none; a frozen or hand-written one might.
		if len(memory.ValidateDraft(draft)) == 0 {
			kept = append(kept, draft)
		}
	}
	if len(kept) == 0 {
		return nil, stripped
	}
	return kept, stripped
}
