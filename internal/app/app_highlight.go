package app

import (
	"encoding/json"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/highlightapp"
)

// Syntax-highlight span RPCs. The backend parses whole documents with
// tree-sitter and returns theme-independent class ids over byte ranges
// (internal/highlight); the frontend maps ids to CSS classes and owns
// all DOM. Highlighting never errors on content: unknown languages,
// over-cap inputs, and parse timeouts all degrade to plain spans, so a
// rejection here means a real failure the frontend must not cache.

func (a *App) highlightService() *highlightapp.Service {
	a.highlightAppOnce.Do(func() {
		a.highlightApp = highlightapp.New(highlightapp.Config{
			Store:          a.store,
			IsShuttingDown: a.shuttingDown.Load,
			ShutdownError:  ErrShuttingDown,
			ResolveContext: func(threadID string, req highlightapp.ContextRequest, maxBytes int64) (string, error) {
				content, _, err := a.diffContextContent("highlight patch with context", threadID, DiffContextRequest{
					Scope: req.Scope, CommitSHA: req.CommitSHA, HeadSHA: req.HeadSHA,
					Path: req.Path, VerifyPatch: req.Patch, EditPayloadID: req.EditPayloadID,
					EditTurnIndex: req.EditTurnIndex,
				}, maxBytes)
				return content, err
			},
			WorkspaceForThread: func(threadID string) (string, error) {
				thread, err := a.store.GetThread(threadID)
				if err != nil {
					return "", err
				}
				_, workspace, err := a.resolveGitPaths(thread)
				return workspace, err
			},
			ReadWorkspaceFile: readWorkspaceFile,
			HasRemoteClient:   a.hasRemoteClient,
			EmitSeed: func(event highlightapp.SeedEvent) {
				a.emit(eventchan.HighlightSeed, wireHighlightSeed(event))
			},
			EmitDiffSeed: func(event highlightapp.DiffSeedEvent) {
				a.emit(eventchan.HighlightDiffSeed, HighlightDiffSeedEvent{ThreadID: event.ThreadID, Files: wirePatchSpanSeeds(event.Files)})
			},
		})
	})
	return a.highlightApp
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
	// Edit selection (edits scope only) — see DiffContextRequest.
	EditPayloadID string `json:"editPayloadId"`
	EditTurnIndex int    `json:"editTurnIndex"`
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
	// Primed: spans were computed with real file content above each
	// hunk. The frontend span cache treats primed entries as strictly
	// better than unprimed ones for the same content (monotonic
	// upgrade, never downgrade).
	Primed bool `json:"primed,omitempty"`
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
	res, err := a.highlightService().Code(req.Lang, req.Source)
	return wireHighlightResult(res), err
}

// HighlightPatch returns patch-aligned spans for one file's unified
// diff: result line i corresponds to patch line i, add/del spans cover
// the prefix-stripped body, context spans include a 1-byte plain pad
// for the kept leading space. Wire-safe.
func (a *App) HighlightPatch(req HighlightPatchRequest) (HighlightResult, error) {
	res, err := a.highlightService().Patch(req.Path, req.Patch)
	return wireHighlightResult(res), err
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
	res, err := a.highlightService().PatchWithContext(threadID, highlightapp.ContextRequest{
		Scope: req.Scope, CommitSHA: req.CommitSHA, HeadSHA: req.HeadSHA,
		Path: req.Path, Patch: req.Patch, EditPayloadID: req.EditPayloadID,
		EditTurnIndex: req.EditTurnIndex,
	})
	return wireHighlightResult(res), err
}

func wireHighlightResult(res highlightapp.Result) HighlightResult {
	return HighlightResult{Lang: res.Lang, Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete, Primed: res.Primed}
}

type HighlightSeedEvent struct {
	ThreadID   string                  `json:"threadId"`
	ItemID     string                  `json:"itemId"`
	Lang       string                  `json:"lang"`
	ContentKey string                  `json:"contentKey,omitempty"`
	LineHashes []uint32                `json:"lineHashes"`
	Lines      []highlight.EncodedLine `json:"lines"`
	Final      bool                    `json:"final"`
}

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
type PersistedPatchSpans struct {
	Version string          `json:"hv"`
	Files   []PatchSpanSeed `json:"files"`
}
type HighlightDiffSeedEvent struct {
	ThreadID string          `json:"threadId"`
	Files    []PatchSpanSeed `json:"files"`
}
type PersistedCodeSpan struct {
	Lang       string                  `json:"lang"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
}
type PersistedCodeSpans struct {
	Version string              `json:"hv"`
	Blocks  []PersistedCodeSpan `json:"blocks"`
}

func wireHighlightSeed(event highlightapp.SeedEvent) HighlightSeedEvent {
	return HighlightSeedEvent{
		ThreadID: event.ThreadID, ItemID: event.ItemID, Lang: event.Lang, ContentKey: event.ContentKey,
		LineHashes: event.LineHashes, Lines: event.Lines, Final: event.Final,
	}
}
func wirePatchSpanSeeds(seeds []highlightapp.PatchSpanSeed) []PatchSpanSeed {
	if len(seeds) == 0 {
		return nil
	}
	out := make([]PatchSpanSeed, len(seeds))
	for i, seed := range seeds {
		out[i] = PatchSpanSeed{Path: seed.Path, ContentKey: seed.ContentKey, Lines: seed.Lines, Primed: seed.Primed}
	}
	return out
}
func (a *App) observeAssistantTextStream(threadID, itemID, text string, final bool) {
	a.highlightService().ObserveAssistantText(threadID, itemID, text, final)
}
func (a *App) observeDiffPayloadPersisted(threadID, payloadID string, previews []string, patch string) {
	a.highlightService().ObserveDiffPayload(threadID, payloadID, previews, patch)
}
func (a *App) buildPersistedCodeSpans(text string) json.RawMessage {
	return a.highlightService().BuildPersistedCodeSpans(text)
}
func (a *App) persistedPayloadPatchSpans(threadID, kind, payloadID string) []PatchSpanSeed {
	if !highlightapp.DiffPayloadKind(kind) {
		return nil
	}
	return wirePatchSpanSeeds(a.highlightService().LoadPatchSpans(threadID, payloadID))
}
func (a *App) loadPersistedPatchSpans(threadID, payloadID string) []PatchSpanSeed {
	return wirePatchSpanSeeds(a.highlightService().LoadPatchSpans(threadID, payloadID))
}

func (a *App) hasRemoteClient() bool {
	if a.remoteClientProbeFn != nil {
		return a.remoteClientProbeFn()
	}
	server := a.transportServer.Load()
	return server != nil && server.HasRemoteClient()
}
