package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/workspacepath"
)

// Diff span persistence: patch-aligned highlight spans for the diff
// surfaces, keyed the way the frontend's diffSpanCache keys them
// (path + contentKey of the per-file patch text). Persisted payloads are
// immutable-per-write, so their spans are a pure function of stored
// content — the triage diff payload observer computes them ONCE at
// persist time and writes them beside the payload row:
//
//   - payloads.preview_spans — spans for the per-file inline-diff
//     preview patches (what DiffFileBlock renders from item meta).
//     Joined into item list reads as Item.PayloadPreviewSpans, so
//     cold-mounted diff cards paint highlighted with zero RPCs.
//   - payloads.spans — spans for the full data blob, attached to
//     GetPayloadData / GetPayloadPreview responses for every caller.
//
// The same worker emits the `highlight:diff_seed` push to every
// client: remote clients get a round-trip head start, and local ones
// upgrade unprimed RPC results in place (persist-time seeds are primed
// with the just-edited file when it still matches — see
// workspaceFilePrimer). Key mismatches (splitter drift vs the frontend
// parser, content replaced after compute) are fail-safe: a stale blob's
// per-file contentKey never matches, the seed misses the cache, and the
// RPC path recomputes. Version skew is caught server-side — a blob
// stamped by another build's schema is never attached — and
// frontend-side for the item-riding preview blobs.

const (
	// diffSeedMaxScanBytes caps the patch text one compute will split
	// at all. Larger payloads persist no spans; each file falls back to
	// the RPC path lazily as it renders.
	diffSeedMaxScanBytes = 4 << 20 // 4 MB

	// diffSeedMaxFileBytes caps one file's patch text. Over it, the
	// file is skipped (RPC path allows far larger inputs).
	diffSeedMaxFileBytes = 256 << 10 // 256 KB

	// diffSeedMaxTotalBytes bounds the aggregate patch text one persist
	// pass highlights; files past the budget degrade to the lazy RPC
	// path, spreading their cost over render time.
	diffSeedMaxTotalBytes = 1 << 20 // 1 MB

	// diffSeedMaxFiles bounds seeds per compute.
	diffSeedMaxFiles = 32

	// diffSeedMaxWorkers bounds concurrent diff-span persist workers.
	// The parse semaphore bounds active tree-sitter work but not
	// goroutines queued behind it; a burst past the cap drops its
	// compute instead of accumulating workers that retain patch
	// strings. A dropped payload simply never gets persisted spans —
	// the RPC path covers it forever after, correct just slower.
	diffSeedMaxWorkers = 4
)

// PatchSpanSeed is one file's precomputed diff spans. ContentKey is the
// frontend `contentKey(patch text)` string; together with Path it is
// exactly the diffSpanCache base key, so the frontend can insert
// without recomputing or re-verifying anything (a stale/mismatched key
// simply never gets looked up).
type PatchSpanSeed struct {
	Path       string                  `json:"path"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
	// Primed: computed with real file content above each hunk (see
	// HighlightResult.Primed). Persist-time seeds are primed when the
	// just-edited workspace file still matched the patch at compute
	// time — the only moment historical-diff spans can be primed
	// correctly, since the file drifts afterward.
	Primed bool `json:"primed,omitempty"`
}

// PersistedPatchSpans is the payloads.preview_spans / payloads.spans
// column value: the seed wire shape, version-stamped.
type PersistedPatchSpans struct {
	// Version is highlight.SchemaVersion() at write time. The payload
	// loads check it server-side before attaching (the wire shape
	// carries no version); the item-riding preview blob is checked by
	// the frontend against its fetched schema version.
	Version string          `json:"hv"`
	Files   []PatchSpanSeed `json:"files"`
}

// HighlightDiffSeedEvent is the `highlight:diff_seed` payload.
type HighlightDiffSeedEvent struct {
	ThreadID string          `json:"threadId"`
	Files    []PatchSpanSeed `json:"files"`
}

// computePatchSpanSeeds splits a unified diff the way the frontend's
// patch parser does and highlights each file within the caps above.
// Results go through the shared content-addressed cache, so the
// frontend's own HighlightPatch request for the same file (a race the
// seed push deliberately tolerates) is a lookup, not a re-parse.
//
// `prime` (nil = never prime) resolves a diff path to current file
// content; a file is parse-primed only when that content still
// byte-matches the patch's new-side lines — the guard against a
// same-file edit landing between persist and this worker's read, so
// stale content degrades to unprimed spans instead of wrong colors.
func (a *App) computePatchSpanSeeds(patch string, prime func(path string) string) []PatchSpanSeed {
	if patch == "" || len(patch) > diffSeedMaxScanBytes {
		return nil
	}
	var seeds []PatchSpanSeed
	total := 0
	for _, seg := range highlight.SplitPatchFiles(patch) {
		if len(seeds) >= diffSeedMaxFiles {
			break
		}
		// Per-file skips run BEFORE the aggregate budget: a file this
		// loop would never highlight must not trip the budget break and
		// starve valid later siblings. Invalid UTF-8 must never seed:
		// JSON transport maps each invalid byte to U+FFFD, so the
		// client's contentKey matches while the spans cover the
		// original byte lengths — the one divergence that would
		// misalign colors instead of missing the cache.
		if len(seg.Patch) > diffSeedMaxFileBytes || !utf8.ValidString(seg.Patch) {
			continue
		}
		if total+len(seg.Patch) > diffSeedMaxTotalBytes {
			break
		}
		total += len(seg.Patch)
		lang := highlight.LangFromPath(seg.Path)
		// Unknown languages still seed: all-plain spans are the
		// backend's authoritative answer and stop the frontend from
		// asking again (same contract as requestFileSpans' cache).
		var content string
		if prime != nil {
			content = prime(seg.Path)
		}
		var res highlight.Result
		primed := false
		if content != "" && highlight.PatchMatchesContent(seg.Patch, content) {
			res = a.highlightCache().PatchWithContext(lang, seg.Patch, content)
			primed = true
		} else {
			res = a.highlightCache().Patch(lang, seg.Patch)
		}
		if res.Incomplete {
			// Transient degradation never seeds or persists — the RPC
			// path owns incomplete retries and their damping.
			continue
		}
		seeds = append(seeds, PatchSpanSeed{
			Path:       seg.Path,
			ContentKey: highlight.FrontendContentKey(seg.Patch),
			Lines:      res.Lines,
			Primed:     primed,
		})
	}
	return seeds
}

// workspaceFilePrimer returns a memoized path → content resolver over
// the thread's workspace for persist-time span priming, or nil when
// the workspace can't be resolved. Unreadable, oversized, or
// workspace-escaping paths (absolute-path edits included) memoize as
// "" — those files simply stay unprimed.
func (a *App) workspaceFilePrimer(threadID string) func(path string) string {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return nil
	}
	contents := map[string]string{}
	return func(path string) string {
		if content, ok := contents[path]; ok {
			return content
		}
		content := ""
		if rel, err := workspacepath.NormalizeRelative(path); err == nil {
			if read, err := readWorkspaceFile(filepath.Join(workspace, rel), highlight.MaxPrimeBytes); err == nil {
				content = read
			}
		}
		contents[path] = content
		return content
	}
}

// observeDiffPayloadPersisted is the triage router's diff payload
// observer (wired in newTriageRouter): a diff-bearing payload was just
// persisted with complete content. May run on the provider read loop —
// compute happens on a goroutine, bounded in count by
// diffSeedMaxWorkers (excess bursts drop; the RPC path covers those
// payloads) and in work by the caps above. The worker computes preview
// AND full-data spans in one pass (parse-primed per file when the
// workspace still matches), writes both columns, and pushes the
// preview seeds on the `highlight:diff_seed` channel so live surfaces
// paint primed spans without a round trip.
func (a *App) observeDiffPayloadPersisted(threadID, payloadID string, previews []string, patch string) {
	if a.diffSeedWorkers.Add(1) > diffSeedMaxWorkers {
		a.diffSeedWorkers.Add(-1)
		return
	}
	go func() {
		defer a.diffSeedWorkers.Add(-1)
		// The moment right after persist is the ONLY time the workspace
		// can prime a historical diff correctly — the file IS the
		// post-edit state (verified per file against the patch, so a
		// racing later edit degrades to unprimed, never wrong colors).
		prime := a.workspaceFilePrimer(threadID)
		var previewSeeds []PatchSpanSeed
		total := 0
		for _, preview := range previews {
			if len(previewSeeds) >= diffSeedMaxFiles {
				break
			}
			// A preview is one file's line-bounded patch; over the
			// per-file cap the compute would skip it anyway, so it must
			// not consume aggregate budget or trip the budget break for
			// later previews.
			if len(preview) > diffSeedMaxFileBytes {
				continue
			}
			if total+len(preview) > diffSeedMaxTotalBytes {
				break
			}
			total += len(preview)
			previewSeeds = append(previewSeeds, a.computePatchSpanSeeds(preview, prime)...)
		}
		// preview_spans rides every item list read, so it gets the same
		// retained-bytes guardrail as the items.meta codeSpans blob;
		// spans is read on demand only and the input caps bound it.
		previewSeeds = capPatchSpanSeedBytes(previewSeeds, persistedCodeSpansMaxBytes)
		// A payload deleted mid-compute (thread deletion racing the
		// worker) must not push seeds either: the frontend's cleanup
		// for that thread may already have run, and a late seed would
		// re-register cache entries for it.
		persisted := a.persistPayloadSpans(payloadID, previewSeeds, a.computePatchSpanSeeds(patch, prime))
		if persisted && len(previewSeeds) > 0 {
			// Pushed to every client (local included): primed seeds are
			// strictly better than what the local RPC path computes for
			// an already-persisted diff, so the frontend upgrades its
			// cache in place when the seed wins the race.
			a.emit("highlight:diff_seed", HighlightDiffSeedEvent{ThreadID: threadID, Files: previewSeeds})
		}
	}()
}

// persistPayloadSpans writes both span blobs against the payload row.
// Returns false only when the payload row no longer exists — the one
// signal the caller must react to (a deleted payload's seeds must not
// be pushed either). A payload deleted between persist and this write
// (thread deletion racing the worker) is otherwise a benign drop;
// any other error is logged — the blobs are an optional accelerator,
// so the persist itself never fails a turn, but a persistent write
// error should be visible.
func (a *App) persistPayloadSpans(payloadID string, previewSeeds, fullSeeds []PatchSpanSeed) bool {
	previewBlob := marshalPersistedPatchSpans(previewSeeds)
	fullBlob := marshalPersistedPatchSpans(fullSeeds)
	if previewBlob == "" && fullBlob == "" {
		// Nothing computed; the persist path already reset both columns
		// when it rewrote the payload row.
		return true
	}
	if err := a.store.UpdatePayloadSpans(payloadID, previewBlob, fullBlob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		log.Printf("app: persist payload spans %s: %v", payloadID, err)
	}
	return true
}

func marshalPersistedPatchSpans(seeds []PatchSpanSeed) string {
	if len(seeds) == 0 {
		return ""
	}
	blob, err := json.Marshal(PersistedPatchSpans{
		Version: highlight.SchemaVersion(),
		Files:   seeds,
	})
	if err != nil {
		log.Printf("app: marshal persisted patch spans: %v", err)
		return ""
	}
	return string(blob)
}

// capPatchSpanSeedBytes enforces a retained-bytes budget over a seed
// slice (same shape formula as encodedLinesBytes). Skip, don't break:
// one giant file must not starve later small ones.
func capPatchSpanSeedBytes(seeds []PatchSpanSeed, budget int) []PatchSpanSeed {
	kept := seeds[:0]
	for _, seed := range seeds {
		cost := encodedLinesBytes(seed.Lines)
		if cost > budget {
			continue
		}
		budget -= cost
		kept = append(kept, seed)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// persistedPayloadPatchSpans loads a diff-kind payload's stored
// full-data span blob for the payload load responses. The version gate
// is server-side: the wire PatchSpans shape carries no version, so a
// blob stamped by another build's schema is dropped here and the
// frontend's RPC path recomputes. Truncated previews are covered by
// per-file content addressing — files fully inside the served prefix
// key identically; a boundary-truncated file misses and RPCs alone.
func (a *App) persistedPayloadPatchSpans(payloadKind, payloadID string) []PatchSpanSeed {
	if !diffPayloadKind(payloadKind) {
		return nil
	}
	return a.loadPersistedPatchSpans(payloadID)
}

// loadPersistedPatchSpans is the kind-free variant for callers whose
// query already filtered to diff-bearing payload kinds (the turn edits
// read).
func (a *App) loadPersistedPatchSpans(payloadID string) []PatchSpanSeed {
	blob, err := a.store.GetPayloadSpans(payloadID)
	if err != nil {
		log.Printf("app: read payload spans %s: %v", payloadID, err)
		return nil
	}
	if blob == "" {
		return nil
	}
	var spans PersistedPatchSpans
	if err := json.Unmarshal([]byte(blob), &spans); err != nil {
		log.Printf("app: decode payload spans %s: %v", payloadID, err)
		return nil
	}
	if spans.Version != highlight.SchemaVersion() || len(spans.Files) == 0 {
		return nil
	}
	return spans.Files
}

// diffPayloadKind reports whether a payload kind's data blob is a
// unified diff: "diff" rows from provider diff events, and
// "tool_result" rows, whose data is always the file-change unified
// patch (possibly empty for summary-only results).
func diffPayloadKind(kind string) bool {
	return kind == "diff" || kind == "tool_result"
}
