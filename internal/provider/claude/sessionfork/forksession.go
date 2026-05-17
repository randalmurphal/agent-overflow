package sessionfork

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// transcriptTypes mirrors _TRANSCRIPT_TYPES in the Python SDK. Entries
// of any other type are non-transcript records (custom-title, ai-title,
// content-replacement, etc.) — they are not copied wholesale into the
// fork; the relevant ones are re-emitted with the new sessionId.
var transcriptTypes = map[string]struct{}{
	"user":       {},
	"assistant":  {},
	"attachment": {},
	"system":     {},
	"progress":   {},
}

// stripFields are fields that would leak source-session state into the
// fork (e.g. team / agent / slug context). Cleared on every forked entry.
var stripFields = []string{
	"teamName",
	"agentName",
	"slug",
	"sourceToolAssistantUUID",
}

// scannerBufInitial / scannerBufMax bound the JSONL line scanner. 16 MB
// max is well above any realistic single-line transcript record (each
// record is one turn / one tool result); the initial 1 MB keeps the
// allocation small for short sessions.
const (
	scannerBufInitial = 1 * 1024 * 1024
	scannerBufMax     = 16 * 1024 * 1024
)

// ErrSessionEmpty is returned when the source JSONL has zero forkable
// entries (no transcript types, or every entry is a sidechain).
var ErrSessionEmpty = errors.New("sessionfork: source session has no messages to fork")

// ErrMessageNotFound is returned when upToMessageUUID does not appear in
// the source transcript.
var ErrMessageNotFound = errors.New("sessionfork: upToMessageUUID not found in source")

// BuildForkLinesWithUUIDMap is the pure transform: reads JSONL from
// src, slices the transcript at upToMessageUUID (inclusive), and
// returns the new session UUID, the JSONL lines that should be
// written to the new session file, and an `oldUUID → newUUID`
// rewrite map produced by the fork transform. customTitle, when
// empty, derives a default ("Forked session (fork)").
//
// upToMessageUUID == "" means clone the full transcript (no slice).
//
// The uuidMap powers the fork-time remap in
// `app_thread_fork.go::remapForkedClaudeUUIDs`, which refreshes AO
// `items.meta` and `thread_checkpoints` rows so a subsequent revert
// lookup in the forked session JSONL finds the cloned user message
// by its current UUID — preserving the invariant "stored
// provider_item_id always matches the active session's UUID."
//
// The returned map covers every transcript entry that survived the
// slice (including non-user types like assistant/system); callers
// only need the user-message entries but the map is unfiltered to
// keep the helper pure.
func BuildForkLinesWithUUIDMap(
	src io.Reader,
	srcSessionID string,
	upToMessageUUID string,
	customTitle string,
) (newSessionID string, lines []string, uuidMap map[string]string, err error) {
	transcript, contentReplacements, err := parseTranscript(src, srcSessionID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sessionfork: parse transcript: %w", err)
	}
	return buildLines(transcript, contentReplacements, srcSessionID, upToMessageUUID, customTitle)
}

// WriteForkFileForLastKeptTurn opens srcPath ONCE, parses the
// transcript in memory, computes the slice point at the end of
// lastKeptTurn (0-indexed) via the ordinal walk, then writes the
// new <newID>.jsonl. Use this only as the legacy fallback when no
// `provider_item_id` is stored on the user_text item — prefer
// `WriteForkFileForUserMessageUUID` because the ordinal walk
// over-counts synthetic user-role entries (`isCompactSummary`,
// `isMeta`, etc. — see `findmessage.go::isRealUserPrompt` for the
// filter the walk applies). Returns the old→new uuid remap so
// callers can refresh AO-stored wire ids
// (`app_thread_fork.go::remapForkedClaudeUUIDs`).
//
// lastKeptTurn < 0 means clear the session entirely — the function
// returns ErrSessionEmpty so the caller can wire the
// no-history-to-keep path explicitly.
func WriteForkFileForLastKeptTurn(
	srcPath string,
	lastKeptTurn int,
	customTitle string,
) (newSessionID string, newPath string, uuidMap map[string]string, err error) {
	if lastKeptTurn < 0 {
		return "", "", nil, ErrSessionEmpty
	}
	srcSessionID := sessionIDFromPath(srcPath)

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("sessionfork: open source: %w", err)
	}
	defer f.Close()

	transcript, contentReplacements, err := parseTranscript(f, srcSessionID)
	if err != nil {
		return "", "", nil, fmt.Errorf("sessionfork: parse transcript: %w", err)
	}

	upToMessageUUID, err := sliceUUIDInTranscript(transcript, lastKeptTurn+1)
	if err != nil {
		return "", "", nil, err
	}

	newID, lines, uuidMap, err := buildLines(transcript, contentReplacements, srcSessionID, upToMessageUUID, customTitle)
	if err != nil {
		return "", "", nil, err
	}

	newID, newPath, err = writeForkOutput(srcPath, newID, lines)
	if err != nil {
		return "", "", nil, err
	}
	return newID, newPath, uuidMap, nil
}

// WriteForkFileForUserMessageUUID opens srcPath ONCE, parses the
// transcript in memory, slices through the parent of the user
// message identified by upToUserMessageUUID, then writes the new
// <newID>.jsonl. This is the structural fix for the ordinal-walk
// off-by-N bug — by matching on the Claude-assigned user message
// UUID stored on the AO `user_text` row's `meta.provider_item_id`
// (or its checkpoint's `provider_user_message_id`), the slice point
// is immune to any number of synthetic user-role entries between
// real prompts.
//
// Returns the old→new uuid remap so the calling fork pipeline can
// refresh AO-stored wire ids on cloned items / checkpoints
// (`app_thread_fork.go::remapForkedClaudeUUIDs`); revert callers
// that aren't forking can discard it.
//
// Returns `ErrMessageNotFound` when upToUserMessageUUID does not
// appear in the source transcript (e.g. the AO row's stored UUID is
// stale relative to the current session JSONL — most often because
// the session was forked but the remap didn't propagate). Callers
// should treat that as a hard error rather than silently falling
// back to the ordinal walk; a wrong-source revert is worse than no
// revert.
func WriteForkFileForUserMessageUUID(
	srcPath string,
	upToUserMessageUUID string,
	customTitle string,
) (newSessionID string, newPath string, uuidMap map[string]string, err error) {
	if upToUserMessageUUID == "" {
		return "", "", nil, fmt.Errorf("sessionfork: empty user message uuid")
	}
	srcSessionID := sessionIDFromPath(srcPath)

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("sessionfork: open source: %w", err)
	}
	defer f.Close()

	transcript, contentReplacements, err := parseTranscript(f, srcSessionID)
	if err != nil {
		return "", "", nil, fmt.Errorf("sessionfork: parse transcript: %w", err)
	}

	upToParentUUID, err := parentUUIDForUserMessageUUIDInTranscript(transcript, upToUserMessageUUID)
	if err != nil {
		return "", "", nil, err
	}
	// upToParentUUID == "" means the user message is the very first
	// real prompt in the transcript — the caller wants to clear the
	// session entirely rather than slice. Mirror the
	// `SliceUUIDForLastKeptTurn(-1)` → ErrSessionEmpty contract so
	// `revertClaudeThreadToMessage` can route through its
	// "checkpoint.TurnIndex == 0" branch identically.
	if upToParentUUID == "" {
		return "", "", nil, ErrSessionEmpty
	}

	newID, lines, uuidMap, err := buildLines(transcript, contentReplacements, srcSessionID, upToParentUUID, customTitle)
	if err != nil {
		return "", "", nil, err
	}

	newID, newPath, err = writeForkOutput(srcPath, newID, lines)
	if err != nil {
		return "", "", nil, err
	}
	return newID, newPath, uuidMap, nil
}

// writeForkOutput writes the JSONL lines to <srcDir>/<newID>.jsonl
// atomically (O_EXCL). On any partial-write failure the output file is
// removed so disk and the caller's notion of the new session stay in
// lockstep.
func writeForkOutput(srcPath, newID string, lines []string) (string, string, error) {
	dir := filepath.Dir(srcPath)
	out := filepath.Join(dir, newID+".jsonl")

	// O_EXCL: refuse to overwrite an existing file. UUIDv4 collisions are
	// vanishingly rare; if it happens, fail rather than clobber.
	fd, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("sessionfork: create %s: %w", out, err)
	}
	w := bufio.NewWriter(fd)
	for _, line := range lines {
		if _, err := w.WriteString(line); err != nil {
			fd.Close()
			_ = os.Remove(out)
			return "", "", fmt.Errorf("sessionfork: write line: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			fd.Close()
			_ = os.Remove(out)
			return "", "", fmt.Errorf("sessionfork: write newline: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		fd.Close()
		_ = os.Remove(out)
		return "", "", fmt.Errorf("sessionfork: flush: %w", err)
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(out)
		return "", "", fmt.Errorf("sessionfork: close: %w", err)
	}
	return newID, out, nil
}

// parentUUIDForUserMessageUUIDInTranscript walks an already-parsed
// transcript and returns the parentUuid of the user prompt whose
// `uuid` matches messageUUID. Mirrors
// `FindParentUUIDForUserMessageUUID` but operates on the in-memory
// slice so the fork pipeline doesn't re-open the file.
//
// Matches on (uuid, type:"user") only — no `isRealUserPrompt`
// filter. The caller supplies a specific UUID stamped by triage on
// the AO `user_text` row, so by construction it's a real prompt;
// re-applying the synthetic-flag filter would be redundant.
// Matching by UUID is also why this path is structurally immune to
// the synthetic-entry over-count bug that motivated the fix —
// counting plays no role.
//
// Returns ErrMessageNotFound when messageUUID is not present in the
// transcript (most often: the AO row's stored UUID is stale relative
// to the current session JSONL — either a remap regression or a
// session that pre-dates the wire-id stamp).
func parentUUIDForUserMessageUUIDInTranscript(transcript []map[string]any, messageUUID string) (string, error) {
	if messageUUID == "" {
		return "", fmt.Errorf("sessionfork: empty user message uuid")
	}
	for _, entry := range transcript {
		if u, _ := entry["uuid"].(string); u != messageUUID {
			continue
		}
		if t, _ := entry["type"].(string); t != "user" {
			continue
		}
		parent, _ := entry["parentUuid"].(string)
		return parent, nil
	}
	return "", fmt.Errorf("%w: user message uuid %q", ErrMessageNotFound, messageUUID)
}

// sliceUUIDInTranscript walks an already-parsed transcript and returns
// the parentUuid of the userTurnIndex-th (0-indexed) real user prompt.
// Mirrors FindUUIDBeforeUserTurn but operates on the in-memory slice
// so callers that already have the transcript don't have to re-read
// the file.
//
// Returns ("", nil) when userTurnIndex == 0 (no preceding entry).
// Returns ErrUserTurnOutOfRange if no Nth real prompt exists.
func sliceUUIDInTranscript(transcript []map[string]any, userTurnIndex int) (string, error) {
	if userTurnIndex == 0 {
		return "", nil
	}
	count := 0
	for _, entry := range transcript {
		if !isRealUserPrompt(entry) {
			continue
		}
		if count == userTurnIndex {
			parent, _ := entry["parentUuid"].(string)
			if parent == "" {
				return "", fmt.Errorf("sessionfork: user turn %d has no parentUuid", userTurnIndex)
			}
			return parent, nil
		}
		count++
	}
	return "", fmt.Errorf("%w: requested %d, found %d", ErrUserTurnOutOfRange, userTurnIndex, count)
}

// sessionIDFromPath extracts the session UUID from a path like
// `~/.claude/projects/<slug>/<uuid>.jsonl`.
func sessionIDFromPath(p string) string {
	base := filepath.Base(p)
	if ext := filepath.Ext(base); ext == ".jsonl" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// parseTranscript splits the JSONL stream into transcript entries (user/
// assistant/attachment/system/progress with a uuid) and content-replacement
// records targeting srcSessionID. Lines that fail to parse are silently
// skipped (mirrors the Python implementation's behavior — a final
// truncated line on a crashing session shouldn't fail the whole fork).
func parseTranscript(r io.Reader, srcSessionID string) (
	transcript []map[string]any,
	contentReplacements []any,
	err error,
) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, scannerBufInitial), scannerBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if jsonErr := json.Unmarshal(line, &entry); jsonErr != nil {
			continue
		}
		t, _ := entry["type"].(string)
		if _, ok := transcriptTypes[t]; ok {
			// Reject empty-string uuids: they'd collide in uuidMap
			// (every empty-uuid entry would map to the same fresh UUID)
			// and break the parentUuid chain walk.
			if id, _ := entry["uuid"].(string); id != "" {
				transcript = append(transcript, entry)
			}
			continue
		}
		if t == "content-replacement" {
			sid, _ := entry["sessionId"].(string)
			if sid != srcSessionID {
				continue
			}
			reps, _ := entry["replacements"].([]any)
			contentReplacements = append(contentReplacements, reps...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}
	return transcript, contentReplacements, nil
}

// buildLines is the core transform — Go port of Python `_build_fork_lines`.
//
// Returns (newSessionID, lines, oldUUID→newUUID map, err). The
// uuidMap covers every transcript entry that survived the slice;
// callers may filter it (e.g. to user-message entries only) before
// passing it to AO-side remap helpers.
func buildLines(
	transcript []map[string]any,
	contentReplacements []any,
	srcSessionID, upToMessageUUID, customTitle string,
) (string, []string, map[string]string, error) {
	// 1. Filter sidechains — subagent transcripts have separate parentUuid
	//    graphs and would corrupt the chain walk. Allocate a fresh backing
	//    slice; sharing with the input would mutate the caller's data and
	//    cause subtle aliasing bugs downstream.
	filtered := make([]map[string]any, 0, len(transcript))
	for _, e := range transcript {
		if v, _ := e["isSidechain"].(bool); !v {
			filtered = append(filtered, e)
		}
	}
	transcript = filtered
	if len(transcript) == 0 {
		return "", nil, nil, ErrSessionEmpty
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
			return "", nil, nil, fmt.Errorf("%w: %s", ErrMessageNotFound, upToMessageUUID)
		}
		transcript = transcript[:cutoff+1]
	}

	// 3. Single pass: remap UUIDs, build byUUID lookup, and partition
	//    writable (transcript minus progress entries) all at once.
	//    Progress entries stay in uuidMap and byUUID because the parent
	//    chain walk traverses them, but they're dropped from `writable`
	//    (SDK doesn't replay UI-only progress lines).
	//
	//    Fresh `writable` backing — sharing with transcript would
	//    corrupt later byUUID/parent reads when the iterator position
	//    overlaps the append cursor.
	uuidMap := make(map[string]string, len(transcript))
	byUUID := make(map[string]map[string]any, len(transcript))
	writable := make([]map[string]any, 0, len(transcript))
	for _, e := range transcript {
		oldUUID, _ := e["uuid"].(string)
		uuidMap[oldUUID] = uuid.NewString()
		byUUID[oldUUID] = e
		if t, _ := e["type"].(string); t != "progress" {
			writable = append(writable, e)
		}
	}
	if len(writable) == 0 {
		return "", nil, nil, ErrSessionEmpty
	}

	forkedSessionID := uuid.NewString()
	now := nowISO()
	lines := make([]string, 0, len(writable)+2)

	for i, original := range writable {
		oldUUID, _ := original["uuid"].(string)
		newUUID := uuidMap[oldUUID]

		newParent := resolveParent(original, byUUID, uuidMap)
		newLogicalParent, hadLogicalParent := resolveLogicalParent(original, uuidMap)

		// Update timestamp only on the LAST writable entry — readers use
		// it for leaf detection on resume. Untouched timestamps preserve
		// real authorship times.
		ts, _ := original["timestamp"].(string)
		if i == len(writable)-1 || ts == "" {
			ts = now
		}

		// Shallow copy preserves unknown fields (cwd, gitBranch, version,
		// promptId, message, ...). We then overwrite the rewritten ones.
		forked := make(map[string]any, len(original)+4)
		for k, v := range original {
			forked[k] = v
		}
		forked["uuid"] = newUUID
		forked["parentUuid"] = newParent
		if hadLogicalParent {
			forked["logicalParentUuid"] = newLogicalParent
		}
		forked["sessionId"] = forkedSessionID
		forked["timestamp"] = ts
		forked["isSidechain"] = false
		forked["forkedFrom"] = map[string]any{
			"sessionId":   srcSessionID,
			"messageUuid": oldUUID,
		}
		for _, k := range stripFields {
			delete(forked, k)
		}

		b, err := json.Marshal(forked)
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal forked entry: %w", err)
		}
		lines = append(lines, string(b))
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
			return "", nil, nil, fmt.Errorf("marshal content-replacement: %w", err)
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
		return "", nil, nil, fmt.Errorf("marshal custom-title: %w", err)
	}
	lines = append(lines, string(b))

	return forkedSessionID, lines, uuidMap, nil
}

// resolveParent walks the parentUuid chain skipping progress ancestors —
// progress entries don't appear in the writable output, so a writable
// entry's effective parent is its first non-progress ancestor.
func resolveParent(
	entry map[string]any,
	byUUID map[string]map[string]any,
	uuidMap map[string]string,
) any {
	parentID, _ := entry["parentUuid"].(string)
	for parentID != "" {
		parent, ok := byUUID[parentID]
		if !ok {
			return nil
		}
		if t, _ := parent["type"].(string); t != "progress" {
			if mapped, ok := uuidMap[parentID]; ok {
				return mapped
			}
			return nil
		}
		parentID, _ = parent["parentUuid"].(string)
	}
	return nil
}

// resolveLogicalParent remaps logicalParentUuid (compact-boundary backpointer).
// Returns (value, true) when the original entry had the field — even if its
// value was null — so the caller knows to write `logicalParentUuid: null`
// rather than omit the field.
func resolveLogicalParent(
	entry map[string]any,
	uuidMap map[string]string,
) (any, bool) {
	raw, ok := entry["logicalParentUuid"]
	if !ok {
		return nil, false
	}
	if s, isStr := raw.(string); isStr && s != "" {
		if mapped, ok := uuidMap[s]; ok {
			return mapped, true
		}
		return s, true // unknown UUID — pass through verbatim
	}
	return raw, true // null passthrough
}

// nowISO returns the current UTC time formatted to match the Python SDK's
// `datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")` output
// shape: millisecond precision, trailing Z. Claude's CLI accepts both
// this and other ISO variants on resume, but the matching shape keeps
// us byte-comparable to a Python-written file.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
