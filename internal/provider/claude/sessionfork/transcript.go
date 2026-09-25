package sessionfork

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

// scannerBufInitial / scannerBufMax bound the JSONL line scanner. 16 MB
// max is well above any realistic single-line transcript record (each
// record is one turn / one tool result); the initial 1 MB keeps the
// allocation small for short sessions.
const (
	scannerBufInitial = 1 * 1024 * 1024
	scannerBufMax     = 16 * 1024 * 1024
)

// TranscriptTypes mirrors _TRANSCRIPT_TYPES in the Python SDK. Entries
// of any other type are non-transcript records (custom-title, ai-title,
// content-replacement, etc.). They are not copied wholesale into the
// fork; the relevant ones are re-emitted with the new sessionId.
//
// Exported because the claude package's branch validator
// (sessionleaf_branch.go) must admit exactly the rows claude's own
// parentUuid walk sees. The fork transform and the validator sharing
// one set is what keeps them in lockstep (invariant 28).
var TranscriptTypes = map[string]struct{}{
	"user":       {},
	"assistant":  {},
	"attachment": {},
	"system":     {},
	"progress":   {},
}

// SessionIDFromPath extracts the session UUID from a path like
// `~/.claude/projects/<slug>/<uuid>.jsonl`.
func SessionIDFromPath(p string) string {
	base := filepath.Base(p)
	if ext := filepath.Ext(base); ext == ".jsonl" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// ParseTranscript splits the JSONL stream into transcript entries (user/
// assistant/attachment/system/progress with a uuid) and content-replacement
// records targeting srcSessionID. Lines that fail to parse are silently
// skipped (mirrors the Python implementation's behavior: a final
// truncated line on a crashing session shouldn't fail the whole fork).
//
// Exported for the session importer, which reads the same files with the
// same admission rules: a second reader would drift from the fork
// transform's idea of what a transcript row is.
func ParseTranscript(r io.Reader, srcSessionID string) (
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
		if _, ok := TranscriptTypes[t]; ok {
			// Reject empty-string uuids: such a row can be neither a slice
			// point nor a parent, and every one of them would collide in
			// the parent index the fork transform walks.
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

// TranscriptParent is the minimum a parent-chain walk needs about one
// transcript row: the parent it records and its own type.
type TranscriptParent struct {
	ParentUUID string
	Type       string
}

// ResolveParentUUID walks the parentUuid chain skipping progress
// ancestors and returns the uuid of the first non-progress ancestor, or
// "" when the row is a chain root or the chain leaves this file.
//
// Exported because the session importer walks SKELETON rows (uuid +
// parent + type + a byte offset, never the decoded entry, since a real
// transcript is too large to hold decoded) and must apply the identical
// rule. Progress rows are transparent to a fork's writable output and to
// the importer's DAG for the same reason; two copies of that walk would
// drift.
func ResolveParentUUID(parentUUID string, lookup func(uuid string) (TranscriptParent, bool)) string {
	for parentUUID != "" {
		parent, ok := lookup(parentUUID)
		if !ok {
			return ""
		}
		if parent.Type != "progress" {
			return parentUUID
		}
		parentUUID = parent.ParentUUID
	}
	return ""
}
