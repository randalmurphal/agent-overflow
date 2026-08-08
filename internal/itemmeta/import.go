package itemmeta

import (
	"fmt"
	"strings"
)

// ImportSourceUUIDKey records which provider-session row an imported
// item was built from. Claude writes the transcript row's own `uuid`;
// Codex has no per-record id and writes `line:<byte offset of the line's
// first byte>`, which an append-only rollout makes just as stable. Both
// providers always write one. It is provenance, not identity: the refresh
// cursor lives on `thread_import_state`, and nothing in the render path
// reads this key.
const ImportSourceUUIDKey = "import_source_uuid"

// ImportUnavailableKey marks an imported item whose payload the provider
// session no longer contains, so the frontend renders "not available
// from import" instead of an empty expandable body. The value is the
// REASON, and the frontend maps it to user-facing copy:
//
//   - "tool-output-gc" — Claude externalised an oversized tool output to
//     `tool-results/*.txt` and later garbage-collected the file.
//   - "exec-detail"    — a Codex exec/patch end-event could not be
//     matched to its tool call unambiguously, so its output and exit
//     status are unknown.
//
// The set is open on purpose: a new import source that loses detail adds
// a reason here and a label in the frontend, and an unmapped reason
// still renders the generic empty branch rather than a lie.
const ImportUnavailableKey = "import_unavailable"

// MarkImported returns raw with the import provenance key set to
// sourceUUID, preserving every existing key. Malformed meta is an error:
// the caller is about to persist this row, and an import that silently
// dropped provenance would leave a thread that cannot be refreshed.
//
// An empty sourceUUID is rejected for the same reason — a blank
// provenance stamp is indistinguishable from a bug at read time.
func MarkImported(raw, sourceUUID string) (string, error) {
	if strings.TrimSpace(sourceUUID) == "" {
		return "", fmt.Errorf("itemmeta: mark imported: empty source uuid")
	}
	merged, err := mergeKey(raw, ImportSourceUUIDKey, sourceUUID)
	if err != nil {
		return "", fmt.Errorf("itemmeta: mark imported: %w", err)
	}
	return merged, nil
}

// MarkImportUnavailable returns raw with the unavailable-payload marker
// set to reason, preserving every existing key. See ImportUnavailableKey
// for the reasons in use. An empty reason is rejected: the frontend
// branches on the value, and a blank one would render an unexplained
// gap.
func MarkImportUnavailable(raw, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("itemmeta: mark import unavailable: empty reason")
	}
	merged, err := mergeKey(raw, ImportUnavailableKey, reason)
	if err != nil {
		return "", fmt.Errorf("itemmeta: mark import unavailable: %w", err)
	}
	return merged, nil
}
