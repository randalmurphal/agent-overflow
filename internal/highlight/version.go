package highlight

import (
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
	"sync"

	"agent-overflow/internal/highlight/grammars"
)

// encodingFormatVersion names the EncodedLine run-pair wire format
// ([byteLen, classId, ...]). Bump only if that format itself changes —
// grammar and class-table changes are picked up automatically below.
const encodingFormatVersion = "enc1"

// patchDocStrategyVersion names how patch hunks are turned into the
// virtual documents that get parsed — the third input (besides grammar
// and class table) that determines span output for a given patch.
// Bump when that construction changes so persisted span blobs computed
// under the old strategy retire instead of pinning stale colors:
// primed blobs are cached "best possible" answers the RPC path never
// re-asks for. v2: primed docs splice the file content BELOW the hunk
// too (an unclosed raw-text element — svelte/html <script> — painted
// its hunks fully plain, and those all-plain blobs persisted as
// primed).
const patchDocStrategyVersion = "patchdoc2"

var (
	schemaVersionOnce sync.Once
	schemaVersion     string
)

// SchemaVersion identifies everything that determines span output for
// a given input: the embedded grammar queries and UPSTREAM pins, the
// class-id table, and the run-length encoding format. Persisted span
// blobs are stamped with it; on mismatch (grammar upgrade, new class,
// format change) stored spans become invisible and the RPC path
// recomputes — the stamp is derived, never hand-bumped.
//
// A grammar C-source bump without an UPSTREAM update is tolerated as
// cosmetic drift: spans always partition their own source text (run
// lengths were computed from the same bytes), so stale spans can only
// color by the old grammar's opinion, never misalign.
func SchemaVersion() string {
	schemaVersionOnce.Do(func() {
		h := fnv.New64a()
		io.WriteString(h, encodingFormatVersion)
		h.Write([]byte{0})
		io.WriteString(h, patchDocStrategyVersion)
		h.Write([]byte{0})
		for _, name := range classNames {
			io.WriteString(h, name)
			h.Write([]byte{0})
		}
		fmt.Fprintf(h, "%x", grammars.SchemaDigest())
		schemaVersion = strconv.FormatUint(h.Sum64(), 16)
	})
	return schemaVersion
}
