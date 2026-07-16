package highlight

import "time"

// Input caps are the primary defense line: a cgo parser crash is
// process-fatal (no recover() across a SIGSEGV), so pathological
// inputs must be bounded out before they reach C. Values mirror the
// frontend's historical TOKENIZE_MAX_LINE_LENGTH and the payload
// preview caps.
const (
	// MaxRequestBytes bounds one RPC's raw input (source or patch
	// text). The transport accepts frames far larger than any
	// legitimate highlight input, so requests beyond this return an
	// empty (all-plain) result with Truncated set before anything is
	// counted, hashed, or allocated per line.
	MaxRequestBytes = 4 << 20 // 4 MB

	// MaxPrimeBytes bounds the resolved file content used to prime
	// patch parsing. Larger files (generated bundles, lock files)
	// fall back to the unprimed result instead of being read, split,
	// and sliced per hunk.
	MaxPrimeBytes = 4 << 20 // 4 MB

	// maxInputBytes bounds any single highlight input (code block,
	// reconstructed diff side, context-primed file). Inputs over the
	// cap return Truncated with plain spans — never an error.
	maxInputBytes = 1 << 20 // 1 MB

	// maxResultLines bounds allocated result lines per request. The
	// frontend treats absent trailing entries as plain, so capping the
	// slice never breaks patch alignment; only inputs averaging under
	// ~16 bytes/line (not code) ever hit it.
	maxResultLines = 1 << 18

	// maxLineBytes bounds a single line; longer lines render plain
	// (minified-file insurance, same rationale as the old worker).
	maxLineBytes = 1000

	// parseTimeout bounds one tree-sitter parse. It must comfortably
	// clear a cap-sized (1 MB) input — ~75k lines of Python parses in
	// the high hundreds of ms on a loaded WSL host — so only
	// adversarial or pathological inputs hit it and degrade to plain
	// text. A timed-out parser is retired, never pooled (see pool.go).
	parseTimeout = time.Second

	// maxInjectionDepth bounds nested language injection (markdown →
	// html → script → …).
	maxInjectionDepth = 3
)

// patchParseBudget bounds one patch's aggregate parse time across
// hunks. Each hunk parses two virtual documents, so a many-hunk patch
// that stays under every per-document cap could otherwise hold a
// semaphore slot through thousands of parses; hunks past the budget
// render plain and the result is flagged Truncated. A var so tests can
// exercise the exhausted-budget branch.
var patchParseBudget = 5 * time.Second
