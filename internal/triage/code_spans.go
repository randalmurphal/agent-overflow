// Package triage — persisted code-span enrichment for assistant text.
// History items are immutable, so highlight spans are a pure function
// of the stored text; persisting them with the item removes highlight
// compute from the frontend's cold-load path entirely (the same
// precedent as the pathRefs allowlist: version-keyed derived metadata,
// never rendered markup).
package triage

import (
	"encoding/json"
	"log"
	"strings"

	"agent-overflow/internal/store"
)

// codeSpansMetaKey is the items.meta key holding the version-stamped
// span blob for the summary's code fences. The value's shape is owned
// by the app layer (PersistedCodeSpans in app_highlight_persist.go);
// triage stores it opaquely.
const codeSpansMetaKey = "codeSpans"

// SetCodeSpanEnricher wires the app-layer builder that turns a settled
// assistant text into the `codeSpans` meta value; nil (the default)
// disables enrichment, and a nil/empty return stores nothing. Unlike
// the stream observers this is a deliberate enrichment contract — its
// output IS persisted with the item — but it must remain a pure
// function of the text: no router state, no influence on any other
// routing decision.
func (r *Router) SetCodeSpanEnricher(fn func(text string) json.RawMessage) {
	r.codeSpanEnricher = fn
}

// enrichCodeSpans merges the enricher's output into item.Meta under
// codeSpansMetaKey. Every assistant_text persist site calls it with
// the FINAL summary already on the item (post interrupted-decoration):
// spans must key to exactly the text the frontend renders. Failures
// skip enrichment — the RPC path covers those fences and the persist
// itself stays robust.
func (r *Router) enrichCodeSpans(item *store.Item) {
	if r.codeSpanEnricher == nil || item.Kind != itemKindAssistantText {
		return
	}
	blob := r.codeSpanEnricher(item.Summary)
	if len(blob) == 0 {
		return
	}
	merged, err := mergeMetaKey(item.Meta, codeSpansMetaKey, blob)
	if err != nil {
		log.Printf("triage: code spans merge meta for %s: %v", item.ID, err)
		return
	}
	item.Meta = merged
}

// mergeMetaKey sets one key in a meta JSON object, preserving raw
// sibling bytes (same round-trip contract as mergePathRefsIntoMeta).
// Unlike the pathRefs merge it never overwrites corrupt meta — an
// unparseable object keeps its bytes and the key is skipped.
func mergeMetaKey(meta, key string, value json.RawMessage) (string, error) {
	obj := map[string]json.RawMessage{}
	trimmed := strings.TrimSpace(meta)
	if trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return "", err
		}
	}
	obj[key] = value
	out, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
