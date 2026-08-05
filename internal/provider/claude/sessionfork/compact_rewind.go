package sessionfork

import "strings"

// The CLI's command-input metadata echo wraps the command name in a
// balanced `<command-name>` pair (see the InjectedUserContentWrappers
// entry in findmessage.go for the full shape and provenance).
const (
	commandNameOpenTag  = "<command-name>"
	commandNameCloseTag = "</command-name>"
)

// compactCommandSliceAnchor decides the slice anchor for a revert/fork
// targeting the user message `anchored`, whose parentUuid is `parent`.
//
// For every ordinary message the anchor is `parent` — slicing there
// keeps everything through the row before the message, which by file
// order includes all of the message's effects. A successful `/compact`
// breaks that assumption: the CLI writes the compaction's effects
// BEFORE the command echo, as the echo's own ancestors —
//
//	compact_boundary (parentUuid:null chain root,
//	  |               logicalParentUuid → pre-compact leaf)
//	  └ summary (isCompactSummary)
//	      └ caveat (isMeta)
//	          └ /compact echo   ← the revert anchor
//
// so slicing at the echo's parent keeps the compacted provider state
// while the caller's timeline deletes the compaction divider — a
// silent context/timeline divergence. Reverting TO the /compact
// message means undoing the compaction, so when the anchored entry is
// a /compact command echo whose kept ancestor chain is compact prelude
// all the way down to a compact_boundary, the anchor rewinds to the
// boundary's logicalParentUuid — the pre-compact leaf.
//
// Anything off-pattern returns `parent` unchanged (the pre-existing
// slice point): a non-/compact anchor, a canceled compaction (no
// boundary in the chain — the CLI writes none on cancel), a boundary
// without a logicalParentUuid, or a logicalParentUuid that doesn't
// resolve in this transcript (the pre-compact leaf predates an earlier
// fork slice). Those are approved degradations to the previous
// behavior, not swallowed errors — the slice still lands exactly where
// it did before this rule existed.
func compactCommandSliceAnchor(transcript []map[string]any, anchored map[string]any, parent string) string {
	if commandEchoName(anchored) != "/compact" {
		return parent
	}
	byUUID := make(map[string]map[string]any, len(transcript))
	for _, e := range transcript {
		if u, _ := e["uuid"].(string); u != "" {
			byUUID[u] = e
		}
	}
	cur := parent
	for range transcript { // bounded so a parentUuid cycle can't spin forever
		e, ok := byUUID[cur]
		if !ok {
			return parent
		}
		if t, _ := e["type"].(string); t == "system" {
			if st, _ := e["subtype"].(string); st != "compact_boundary" {
				return parent
			}
			lp, _ := e["logicalParentUuid"].(string)
			if lp == "" {
				return parent
			}
			if _, ok := byUUID[lp]; !ok {
				return parent
			}
			return lp
		}
		if !isCompactPreludeRow(e) {
			return parent
		}
		cur, _ = e["parentUuid"].(string)
		if cur == "" {
			return parent
		}
	}
	return parent
}

// isCompactPreludeRow reports whether entry is one of the synthetic
// user rows the CLI writes between a compact_boundary and the /compact
// command echo: the compacted summary (isCompactSummary) and the
// local-command caveat (isMeta). Matching the flags rather than the
// exact two observed shapes keeps the walk robust if the CLI adds
// another injected row to the prelude; any row without a synthetic
// flag terminates the walk in the caller.
func isCompactPreludeRow(entry map[string]any) bool {
	if t, _ := entry["type"].(string); t != "user" {
		return false
	}
	if v, _ := entry["isCompactSummary"].(bool); v {
		return true
	}
	v, _ := entry["isMeta"].(bool)
	return v
}

// commandEchoName extracts the slash-command name from a command-input
// metadata echo row (`<command-name>/compact</command-name>…`).
// Returns "" when the entry is not a user row or its content carries
// no balanced command-name wrapper. Both persisted content shapes are
// handled: the string form the CLI writes today and array content with
// text blocks.
func commandEchoName(entry map[string]any) string {
	if t, _ := entry["type"].(string); t != "user" {
		return ""
	}
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return ""
	}
	switch content := msg["content"].(type) {
	case string:
		return commandNameFromText(content)
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := b["type"].(string); t != "text" {
				continue
			}
			text, _ := b["text"].(string)
			if name := commandNameFromText(text); name != "" {
				return name
			}
		}
	}
	return ""
}

// commandNameFromText pulls the trimmed inner text of the first
// balanced `<command-name>` pair in s, or "" when either half is
// missing.
func commandNameFromText(s string) string {
	open := strings.Index(s, commandNameOpenTag)
	if open < 0 {
		return ""
	}
	rest := s[open+len(commandNameOpenTag):]
	end := strings.Index(rest, commandNameCloseTag)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
