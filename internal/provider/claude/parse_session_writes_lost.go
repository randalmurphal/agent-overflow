package claude

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/provider"
)

// Bounds on the wire lists of system/session_writes_lost. The CLI's lists
// are short (two known `lost` values, usually one session id); the caps keep
// a malformed envelope from growing the error row's meta.
const (
	maxSessionWritesLostEntries = 16
	maxSessionWritesLostRunes   = 128
)

// parseSessionWritesLostEvent handles `system/session_writes_lost`
// (2.1.317+; schema read from the 2.1.321 bundle): the CLI could not save
// lines of a session transcript, its ~/.claude.json, or both, because the
// disk was full (`ENOSPC`) or over quota (`EDQUOT`). The CLI sends it at
// most once, as it exits with status 0, after the last result.
//
// It becomes a non-fatal EventError, so triage persists one error row on
// the thread's last turn. The meta carries no top-level `fatal` or `error`:
// the results already delivered stand. Both enums are open on the wire;
// an unknown code is named in the text and an unknown `lost` value is kept
// in the meta only.
func parseSessionWritesLostEvent(
	threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
	line []byte,
) []provider.ProviderEvent {
	errorCode := boundedClaudeFallbackField(readRawString(raw["error_code"]), maxSessionWritesLostRunes)
	lost := boundedStringList(raw["lost"])
	sessionIDs := boundedStringList(raw["transcript_session_ids"])

	meta := map[string]any{"kind": "session_writes_lost"}
	if errorCode != "" {
		meta["errorCode"] = errorCode
	}
	if len(lost) > 0 {
		meta["lost"] = lost
	}
	if len(sessionIDs) > 0 {
		meta["transcriptSessionIds"] = sessionIDs
	}
	encoded, _ := json.Marshal(meta)

	return []provider.ProviderEvent{{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   sessionWritesLostText(errorCode, lost),
		Meta:      encoded,
		Timestamp: now,
		Raw:       line,
	}}
}

func sessionWritesLostText(errorCode string, lost []string) string {
	transcript := slices.Contains(lost, "transcript")
	config := slices.Contains(lost, "global_config")
	var subject string
	switch {
	case transcript && config:
		subject = "the conversation transcript and its settings file (~/.claude.json)"
	case transcript:
		subject = "the conversation transcript"
	case config:
		subject = "its settings file (~/.claude.json)"
	default:
		subject = "some of its session data"
	}
	var cause string
	switch errorCode {
	case "ENOSPC":
		cause = "the disk is full"
	case "EDQUOT":
		cause = "the disk quota is used up"
	case "":
		cause = "the disk is full or over quota"
	default:
		cause = fmt.Sprintf("a disk error (%s)", errorCode)
	}
	text := fmt.Sprintf("Claude could not save %s because %s.", subject, cause)
	if transcript {
		text += " Resuming this conversation may miss part of it."
	}
	return text
}

// boundedStringList decodes a JSON string array, dropping blank and
// non-string entries and bounding the count and each entry. Absent or
// malformed is nil.
func boundedStringList(raw json.RawMessage) []string {
	var values []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	var out []string
	for _, value := range values {
		if len(out) == maxSessionWritesLostEntries {
			break
		}
		if entry := boundedClaudeFallbackField(readRawString(value), maxSessionWritesLostRunes); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
