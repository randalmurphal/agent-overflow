package sessionfork

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// stripFields are fields that would leak source-session state into the
// fork (e.g. team / agent / slug context). Cleared on every forked entry.
var stripFields = []string{
	"teamName",
	"agentName",
	"slug",
	"sourceToolAssistantUUID",
}

// BuildForkLines is the pure transform: reads JSONL from src, slices the
// transcript at upToMessageUUID (inclusive), and returns the new session
// id and the JSONL lines to write to the new session file. customTitle,
// when empty, derives a default ("Forked session (fork)").
//
// upToMessageUUID == "" means clone the full transcript (no slice).
func BuildForkLines(
	src io.Reader,
	srcSessionID string,
	upToMessageUUID string,
	customTitle string,
) (newSessionID string, lines []string, err error) {
	transcript, contentReplacements, err := ParseTranscript(src, srcSessionID)
	if err != nil {
		return "", nil, fmt.Errorf("sessionfork: parse transcript: %w", err)
	}
	return buildLines(transcript, contentReplacements, srcSessionID, upToMessageUUID, customTitle)
}

// buildLines is the core transform, a Go port of the Python SDK's
// `_build_fork_lines` with one deliberate difference: every entry keeps
// its uuid. The Claude CLI's own fork copies (`--fork-session` and
// `/branch`) preserve source uuids verbatim, so one uuid in two session
// files is the CLI's normal state, and every provider id AO stored
// against the source names the same row in the new file.
//
// Only session identity changes: `sessionId` on every line, the
// `forkedFrom` provenance stamp (the shape `/branch` writes; the session
// importer reads `forkedFrom.sessionId` as the parent session), and the
// new file's own content-replacement and custom-title rows.
//
// parentUuid changes in exactly two structural cases. Progress rows are
// dropped from the output, so a row whose parent is a progress row
// chains to its first non-progress ancestor. A deferred
// `system/api_error` row chains to its file predecessor (rechain.go).
// logicalParentUuid is copied verbatim.
func buildLines(
	transcript []map[string]any,
	contentReplacements []any,
	srcSessionID, upToMessageUUID, customTitle string,
) (string, []string, error) {
	// 1. Filter sidechains: subagent transcripts have separate parentUuid
	//    graphs and would corrupt the chain walk. A fresh backing slice
	//    keeps the caller's transcript unmodified.
	filtered := make([]map[string]any, 0, len(transcript))
	for _, e := range transcript {
		if v, _ := e["isSidechain"].(bool); !v {
			filtered = append(filtered, e)
		}
	}
	transcript = filtered
	if len(transcript) == 0 {
		return "", nil, ErrSessionEmpty
	}

	// 2. Slice up to upToMessageUUID inclusive.
	if upToMessageUUID != "" {
		cutoff := -1
		for i, e := range transcript {
			if u, _ := e["uuid"].(string); u == upToMessageUUID {
				cutoff = i
				break
			}
		}
		if cutoff == -1 {
			return "", nil, fmt.Errorf("%w: %s", ErrMessageNotFound, upToMessageUUID)
		}
		transcript = transcript[:cutoff+1]
	}

	// 3. Index every kept row and collect the writable ones. Progress
	//    rows stay in the index because the parent walk traverses them,
	//    but are dropped from the output (the SDK does not replay
	//    UI-only progress lines).
	byUUID := make(map[string]map[string]any, len(transcript))
	writable := make([]map[string]any, 0, len(transcript))
	for _, e := range transcript {
		id, _ := e["uuid"].(string)
		byUUID[id] = e
		if t, _ := e["type"].(string); t != "progress" {
			writable = append(writable, e)
		}
	}
	if len(writable) == 0 {
		return "", nil, ErrSessionEmpty
	}

	forkedSessionID := uuid.NewString()
	now := nowISO()
	lines := make([]string, 0, len(writable)+2)

	var prevWritableUUID string
	for i, original := range writable {
		id, _ := original["uuid"].(string)

		parent := resolveParent(original, byUUID)
		if i > 0 && isDeferredAPIErrorRow(original) {
			// Deferred api_error rows carry a known-stale parentUuid
			// (written at next-send with the retry-time leaf, bypassing
			// the rest of the turn). Force-chain them at their file
			// position so the fork's tail stays on the active branch.
			// This is a no-op when the source row was already chained to
			// its predecessor. See rechain.go for the full contract.
			parent = prevWritableUUID
		}

		// Update timestamp only on the LAST writable entry: readers use
		// it for leaf detection on resume. Untouched timestamps preserve
		// real authorship times.
		ts, _ := original["timestamp"].(string)
		if i == len(writable)-1 || ts == "" {
			ts = now
		}

		// Shallow copy preserves every field not rewritten below (uuid,
		// logicalParentUuid, cwd, gitBranch, version, promptId,
		// message, ...).
		forked := make(map[string]any, len(original)+2)
		for k, v := range original {
			forked[k] = v
		}
		forked["parentUuid"] = parent
		forked["sessionId"] = forkedSessionID
		forked["timestamp"] = ts
		forked["isSidechain"] = false
		forked["forkedFrom"] = map[string]any{
			"sessionId":   srcSessionID,
			"messageUuid": id,
		}
		for _, k := range stripFields {
			delete(forked, k)
		}

		b, err := json.Marshal(forked)
		if err != nil {
			return "", nil, fmt.Errorf("marshal forked entry: %w", err)
		}
		lines = append(lines, string(b))
		prevWritableUUID = id
	}

	if len(contentReplacements) > 0 {
		entry := map[string]any{
			"type":         "content-replacement",
			"sessionId":    forkedSessionID,
			"replacements": contentReplacements,
			"uuid":         uuid.NewString(),
			"timestamp":    now,
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return "", nil, fmt.Errorf("marshal content-replacement: %w", err)
		}
		lines = append(lines, string(b))
	}

	title := customTitle
	if title == "" {
		title = "Forked session (fork)"
	}
	titleEntry := map[string]any{
		"type":        "custom-title",
		"sessionId":   forkedSessionID,
		"customTitle": title,
		"uuid":        uuid.NewString(),
		"timestamp":   now,
	}
	b, err := json.Marshal(titleEntry)
	if err != nil {
		return "", nil, fmt.Errorf("marshal custom-title: %w", err)
	}
	lines = append(lines, string(b))

	return forkedSessionID, lines, nil
}

// resolveParent returns the parentUuid a writable row keeps in the fork:
// its first non-progress ancestor among the kept rows, or nil when the
// row is a chain root or its chain leaves the kept rows. Progress rows
// never reach the output, so a parent that names one would dangle.
func resolveParent(entry map[string]any, byUUID map[string]map[string]any) any {
	parentID, _ := entry["parentUuid"].(string)
	resolved := ResolveParentUUID(parentID, func(u string) (TranscriptParent, bool) {
		parent, ok := byUUID[u]
		if !ok {
			return TranscriptParent{}, false
		}
		next, _ := parent["parentUuid"].(string)
		typ, _ := parent["type"].(string)
		return TranscriptParent{ParentUUID: next, Type: typ}, true
	})
	if resolved == "" {
		return nil
	}
	return resolved
}

// nowISO returns the current UTC time formatted to match the Python SDK's
// `datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")` output
// shape: millisecond precision, trailing Z. Claude's CLI accepts both
// this and other ISO variants on resume, but the matching shape keeps
// us byte-comparable to a Python-written file.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
