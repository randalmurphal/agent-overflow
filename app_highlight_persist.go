package main

import (
	"encoding/json"
	"log"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
)

// Persisted highlight spans: history items are immutable, so their
// spans are a pure function of stored content. Triage persists them
// WITH the item (version-stamped) and the frontend ingests them at row
// mount — cold loads (thread open, scroll-up, restarts) paint
// highlighted with zero RPCs and zero backend parses. Same fail-safe
// key discipline as the live seed push: a blob whose contentKey or
// schema version doesn't match what the client computes is simply
// never used, and the RPC path recomputes.

// persistedCodeSpansMaxBytes bounds the estimated span payload stored
// on one item's meta. Meta rides every item-list load, so a
// pathological all-code message must not attach megabytes of runs;
// fences past the budget fall back to the RPC path lazily. Estimated
// with the same shape formula the frontend caches use — the bound is a
// guardrail, not an exact wire size.
const persistedCodeSpansMaxBytes = 256 << 10

// PersistedCodeSpan is one fence's spans inside the items.meta
// `codeSpans` value. Lang is the first whitespace-delimited word of
// the fence info string and ContentKey the frontend
// `contentKey(source)` — together exactly the codeSpanCache key, so
// ingest is a plain insert with no recomputation.
type PersistedCodeSpan struct {
	Lang       string                  `json:"lang"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
}

// PersistedCodeSpans is the items.meta `codeSpans` value.
type PersistedCodeSpans struct {
	// Version is highlight.SchemaVersion() at write time; the frontend
	// ignores blobs stamped with anything but its connected backend's
	// version.
	Version string              `json:"hv"`
	Blocks  []PersistedCodeSpan `json:"blocks"`
}

// buildPersistedCodeSpans is the triage code-span enricher (wired in
// newTriageRouter): spans for every fence of a settled assistant text.
// Runs on triage settle goroutines (and the rare whole-block persist
// on the read loop, which already pays SQLite writes there); compute
// is capped and usually a shared-cache lookup — the seed observer
// parsed the same fences moments earlier whenever a remote client was
// attached.
func (a *App) buildPersistedCodeSpans(text string) json.RawMessage {
	if text == "" || len(text) > seedMaxScanBytes {
		return nil
	}
	var blocks []PersistedCodeSpan
	budget := persistedCodeSpansMaxBytes
	for _, fence := range highlight.ScanFences(text) {
		// Same guards as the seed push: languageless fences render
		// plain without a request; oversized fences use the RPC path;
		// invalid UTF-8 must never be content-addressed across a JSON
		// boundary (the U+FFFD mapping would match keys while spans
		// cover the original byte lengths — misaligned colors, not a
		// miss). A trailing unclosed fence IS included: the settled
		// summary is final content and renders as-is.
		if fence.Lang == "" || len(fence.Source) > seedMaxSourceBytes ||
			!utf8.ValidString(fence.Source) {
			continue
		}
		res := a.highlightCache().Code(highlight.LangFromName(fence.Lang), fence.Source)
		if res.Incomplete {
			// Transient degradation must not persist for the item's
			// lifetime; the RPC path owns retries.
			continue
		}
		cost := encodedLinesBytes(res.Lines)
		if cost > budget {
			// Skip, don't break: one giant fence must not starve
			// later small ones.
			continue
		}
		budget -= cost
		blocks = append(blocks, PersistedCodeSpan{
			Lang:       fence.Lang,
			ContentKey: highlight.FrontendContentKey(fence.Source),
			Lines:      res.Lines,
		})
	}
	if len(blocks) == 0 {
		return nil
	}
	blob, err := json.Marshal(PersistedCodeSpans{
		Version: highlight.SchemaVersion(),
		Blocks:  blocks,
	})
	if err != nil {
		log.Printf("app: marshal persisted code spans: %v", err)
		return nil
	}
	return blob
}

// encodedLinesBytes estimates a span array's retained size (same shape
// formula as the frontend caches: per-line overhead + per-run pair).
func encodedLinesBytes(lines []highlight.EncodedLine) int {
	bytes := 0
	for _, line := range lines {
		bytes += 8 + len(line.Runs)*4
	}
	return bytes
}
