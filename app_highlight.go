package main

import (
	"agent-overflow/internal/highlight"
)

// Syntax-highlight span RPCs. The backend parses whole documents with
// tree-sitter and returns theme-independent class ids over byte ranges
// (internal/highlight); the frontend maps ids to CSS classes and owns
// all DOM. Highlighting never errors on content: unknown languages,
// over-cap inputs, and parse timeouts all degrade to plain spans, so a
// rejection here means a real failure the frontend must not cache.

// highlightCache returns the lazy-init content-addressed span cache.
func (a *App) highlightCache() *highlight.Cache {
	a.highlightCacheOnce.Do(func() {
		a.highlightSpanCache = highlight.NewCache()
	})
	return a.highlightSpanCache
}

// HighlightCodeRequest carries raw text (markdown code blocks, any
// free-standing source) plus its language name (markdown fence info
// string or canonical name; unknown names render plain).
type HighlightCodeRequest struct {
	Lang   string `json:"lang"`
	Source string `json:"source"`
}

// HighlightPatchRequest carries one file's unified diff exactly as the
// frontend's patch parser splits it; Path drives language detection.
type HighlightPatchRequest struct {
	Path  string `json:"path"`
	Patch string `json:"patch"`
}

// HighlightPatchContextRequest is HighlightPatchRequest plus the
// review-pane scope fields (same shape as DiffContextRequest) that
// resolve the new-side file content used to prime parsing above each
// hunk.
type HighlightPatchContextRequest struct {
	Scope     string `json:"scope"`
	CommitSHA string `json:"commitSHA"`
	HeadSHA   string `json:"headSHA"`
	Path      string `json:"path"`
	Patch     string `json:"patch"`
}

// HighlightResult is the span payload. Lines align 1:1 with the input
// text's newline-split lines (for patches: the patch text's own line
// sequence, meta lines plain). Each line's runs are flat
// [byteLen, classId, ...] pairs over the line's UTF-8 bytes; empty
// runs mean a plain line. Truncated flags inputs past the size cap
// (the overflow renders plain). Incomplete flags transient degradation
// (parse timeout, patch budget) — the backend cache skips these so a
// retry can succeed, and frontend caches must apply the same
// transient-vs-permanent distinction instead of memoizing the partial
// result for the session.
type HighlightResult struct {
	Lang       string                  `json:"lang"`
	Lines      []highlight.EncodedLine `json:"lines"`
	Truncated  bool                    `json:"truncated"`
	Incomplete bool                    `json:"incomplete"`
}

// HighlightClassNames returns the classId → semantic-name table
// (index = class id, 0 = "none"). The frontend fetches it once at boot
// and renders class id N as CSS class `syntax-<name>`. Wire-safe.
func (a *App) HighlightClassNames() []string {
	return highlight.ClassNames()
}

// HighlightSchemaVersion returns the version stamp carried by persisted
// span blobs (items.meta codeSpans, payload preview/full spans). The
// frontend fetches it once at boot and ignores stored spans stamped
// with anything else — those fall back to the RPC path, which
// recomputes under the current schema. Wire-safe.
func (a *App) HighlightSchemaVersion() string {
	return highlight.SchemaVersion()
}

// HighlightCode returns spans for raw source text. Wire-safe: pure
// text-in/metadata-out, no local state touched.
func (a *App) HighlightCode(req HighlightCodeRequest) (HighlightResult, error) {
	if a.shuttingDown.Load() {
		return HighlightResult{}, ErrShuttingDown
	}
	lang := highlight.LangFromName(req.Lang)
	if len(req.Source) > highlight.MaxRequestBytes {
		// Degrade before hashing or counting a transport-limit-sized
		// frame; absent lines render plain.
		return HighlightResult{Lang: lang.String(), Truncated: true}, nil
	}
	res := a.highlightCache().Code(lang, req.Source)
	return HighlightResult{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete}, nil
}

// HighlightPatch returns patch-aligned spans for one file's unified
// diff: result line i corresponds to patch line i, add/del spans cover
// the prefix-stripped body, context spans include a 1-byte plain pad
// for the kept leading space. Wire-safe.
func (a *App) HighlightPatch(req HighlightPatchRequest) (HighlightResult, error) {
	if a.shuttingDown.Load() {
		return HighlightResult{}, ErrShuttingDown
	}
	lang := highlight.LangFromPath(req.Path)
	if len(req.Patch) > highlight.MaxRequestBytes {
		return HighlightResult{Lang: lang.String(), Truncated: true}, nil
	}
	res := a.highlightCache().Patch(lang, req.Patch)
	return HighlightResult{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete}, nil
}

// HighlightPatchWithContext is HighlightPatch primed with the file
// content above each hunk, resolved through the same scope switch as
// GetDiffContextLines — a hunk that starts mid-docstring highlights
// correctly because the parser has seen the opening. Content
// resolution is best-effort: if the scope lookup fails (file gone at
// ref, no local clone), the unprimed result is returned instead of an
// error. LocalOnlyMethods category 1: it reads workspace/ref file
// content by path; remote clients use HighlightPatch.
func (a *App) HighlightPatchWithContext(threadID string, req HighlightPatchContextRequest) (HighlightResult, error) {
	const action = "highlight patch with context"
	if a.shuttingDown.Load() {
		return HighlightResult{}, ErrShuttingDown
	}
	lang := highlight.LangFromPath(req.Path)
	if len(req.Patch) > highlight.MaxRequestBytes {
		return HighlightResult{Lang: lang.String(), Truncated: true}, nil
	}
	content, err := a.diffContextContent(action, threadID, DiffContextRequest{
		Scope:     req.Scope,
		CommitSHA: req.CommitSHA,
		HeadSHA:   req.HeadSHA,
		Path:      req.Path,
	}, highlight.MaxPrimeBytes)
	var res highlight.Result
	if err != nil || content == "" {
		res = a.highlightCache().Patch(lang, req.Patch)
	} else {
		res = a.highlightCache().PatchWithContext(lang, req.Patch, content)
	}
	return HighlightResult{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete}, nil
}
