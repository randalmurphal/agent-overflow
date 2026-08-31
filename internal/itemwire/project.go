package itemwire

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/store"
)

// Byte budgets for one item's projection. Every one of them is a
// CEILING above which a field stops riding the item list, not a target:
// under budget the stored bytes go out untouched, so the common row is
// byte-identical to what it was before this projection existed.
//
// The figures come from the 2026-08-30 measurement in
// docs/specs/remote-access.md §14 (a 200-row cold window on a real
// 65,877-item thread: 330 KB raw, 109 tool_call rows carrying 81% of
// it).
const (
	// MetaMaxBytes is the encoded `items.meta` size above which
	// `meta.input` elision runs. The measured window averages ~580
	// bytes of `meta` per tool_call row, so 2 KiB leaves the ordinary
	// row alone and reaches only the tail of the distribution — the
	// multi-KiB `Write.content`, `Edit.new_string` and `Task.prompt`
	// values that are 59 KB of that window and paint nothing on
	// arrival.
	MetaMaxBytes = 2 << 10

	// LeafFloorBytes is the smallest `meta.input` leaf the projection
	// will drop. Outside the identity keys retained below, every surface
	// that renders such a leaf caps its own output far under this (80
	// runes for the persisted summary preview, 120 for an MCP argument
	// list, 60 per MCP value, 160 for a collab prompt preview), so no
	// leaf small enough to be rendered whole is ever a candidate and the
	// projection cannot change a pixel through them. The frontend keeps
	// that inventory honest with a tripwire rather than trusting it:
	// metaInputLeafRenderCaps.test.ts.
	LeafFloorBytes = 1 << 10

	// InlinePreviewMaxBytes is the per-item ceiling on inline diff
	// preview text. `collapseDiffPreviews` defaults on, so the usual
	// client asks for no previews at all and this bound never applies;
	// it exists for the client that does render them, where a
	// 25-file row could otherwise attach ~60 KB of patch text to one
	// item. One to three file edits — the common shape — stay whole.
	InlinePreviewMaxBytes = 16 << 10
)

// MarkerKey is the reserved `items.meta` key carrying the projection's
// typed marker. It only ever appears on the wire: nothing writes it to
// SQLite, and a stored meta that somehow contained one would be
// overwritten by the projection rather than trusted.
const MarkerKey = "wireElision"

// Elision names what the projection removed from one item, so a client
// can tell "this value is not set" from "this value did not fit", and
// so the recovery route has something to be asked for.
type Elision struct {
	// Input lists the dropped `meta.input` leaves by path, e.g.
	// `content` or `edits/0/new_string`.
	Input []string `json:"input,omitempty"`
}

// A tool call's `meta.input` carries two kinds of value: the CONTENT it
// acted with — file bodies, replacement strings, prompts, commands —
// and the IDENTITY of what it acted on: the path, the skill, the query,
// the recipient, the questions asked. The bytes are all in the first
// kind; every row's header renders the second kind whole.
//
// So the size rule governs content, and identity is retained whatever
// its size. That is not a field allowlist standing in for the size rule
// — it is the boundary the size rule was always approximating, named
// explicitly. Retaining identity costs the wire nothing measurable: a
// path is tens of bytes, and no window is over budget because of them.
//
// Every other `meta.input` reader caps its own output far below
// LeafFloorBytes (120 bytes for an MCP argument list, 60 per MCP value,
// 160 for a collab preview, 80 runes for the persisted summary the
// generic path falls back to), so no leaf small enough to be rendered
// whole is a candidate. The frontend tripwire
// (metaInputLeafRenderCaps.test.ts) walks BOTH halves of that claim: it
// fails if a capped reader loses its cap, and it fails if an uncapped
// reader appears that this list does not name.
//
// Keys, not paths: identity appears at the top of `input` for every
// provider shape observed, and a key match retains the subtree beneath
// it (`files[]`, `questions[]/question`).
var retainedIdentityKeys = []string{
	// What was acted on. NotebookEdit spells it `notebook_path`; Codex
	// multi-file edits carry `files` (frontend
	// structuredFileEditPathTargets renders every entry).
	"file_path",
	"notebook_path",
	"path",
	"files",
	// What was asked for. `skill` and `query` go straight into the row
	// preview; `recipient`/`to` name the addressee of a SendMessage,
	// which the row exists to report and nothing else records.
	"skill",
	"query",
	"recipient",
	"to",
	// The AskUserQuestion card's whole source. `extractQuestions`
	// requires each `question` to be a string, so an elided one does
	// not shorten the card — it deletes the question.
	"questions",
	// The command line IS the command row. Retained conditionally, see
	// below.
	"command",
}

// `commandTextForItem` (frontend commandDisplay.ts) reads
// `payloadMeta.command` before `meta.input.command`, and triage stamps
// that copy onto every command row that produced output. For those rows
// the meta leaf is a duplicate of a string the same item already ships,
// so dropping it renders byte-identically — the one identity key with a
// second copy to fall back on, and the one that is regularly large
// enough to matter.
const retainedCommandPath = "command"

var (
	retainIdentity            = identityRetainSet(true)
	retainIdentityLessCommand = identityRetainSet(false)
)

func identityRetainSet(withCommand bool) map[string]bool {
	set := make(map[string]bool, len(retainedIdentityKeys))
	for _, key := range retainedIdentityKeys {
		if key == retainedCommandPath && !withCommand {
			continue
		}
		set[key] = true
	}
	return set
}

// Project returns the item as a client should receive it. The argument
// is a value copy and the returned item is a value copy, so the caller's
// row — and the stored row behind it — are never mutated.
//
// inlinePreviews is the client's stated preference: false means it
// renders diff previews behind a chevron (`collapseDiffPreviews`, the
// default) and none of the patch text paints on arrival. The server
// never reads that setting itself; it rides the request.
func Project(item store.Item, inlinePreviews bool) store.Item {
	item.Meta = ProjectMeta(item.Meta, item.PayloadMeta)
	projected, elidedAny, keptAny := ProjectPayloadMeta(item.PayloadMeta, inlinePreviews)
	item.PayloadMeta = projected
	if elidedAny && !keptAny {
		// preview_spans indexes the preview patches by path. With
		// every patch gone the blob describes nothing the client can
		// render, and an empty value already means "not computed, use
		// the highlight RPC" (store.Item.PayloadPreviewSpans), so the
		// existing fallback covers it with no new contract.
		item.PayloadPreviewSpans = ""
	}
	return item
}

// ProjectItems projects a slice in place and returns it. The slice is
// the caller's own freshly-read page, never a shared cache.
func ProjectItems(items []store.Item, inlinePreviews bool) []store.Item {
	for i := range items {
		items[i] = Project(items[i], inlinePreviews)
	}
	return items
}

// ProjectMeta bounds one `items.meta` value. Under budget it returns
// the stored string untouched — the fast path, one length check, no
// parse and no allocation, which is what lets this sit on the live
// item-upsert path.
func ProjectMeta(meta string, payloadMeta string) string {
	if len(meta) <= MetaMaxBytes {
		return meta
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(meta), &top) != nil || top == nil {
		return meta
	}
	input, ok := top["input"]
	if !ok {
		// The oversized bytes are somewhere else on this meta (a tool
		// echo, a collab state list). Those have their own bounds on
		// the PERSIST path (internal/itemmeta), which is where a
		// shape nobody projects belongs.
		return meta
	}

	// Shared, never mutated: the two shapes are picked, not built, so
	// an over-budget row costs no map allocation.
	retain := retainIdentity
	if hasKey(payloadMeta, retainedCommandPath) {
		retain = retainIdentityLessCommand
	}
	projected, dropped := elideLargestLeaves(input, len(meta)-MetaMaxBytes, LeafFloorBytes, retain)
	if len(dropped) == 0 {
		return meta
	}

	marker, err := json.Marshal(Elision{Input: dropped})
	if err != nil {
		return meta
	}
	top["input"] = projected
	top[MarkerKey] = marker
	out, err := json.Marshal(top)
	if err != nil {
		return meta
	}
	return string(out)
}

// ProjectPayloadMeta bounds the inline diff previews on one
// `payloadMeta` value, returning the projected string plus whether any
// preview was dropped and whether any survived.
//
// A dropped preview keeps its file entry whole — path, rename source,
// change kind, insertion and deletion counts, the truncation flag — so
// the collapsed row renders exactly as it did. Only the patch text
// goes, under a `previewElided` marker the expanded card reads to fetch
// it back.
func ProjectPayloadMeta(payloadMeta string, inlinePreviews bool) (string, bool, bool) {
	// Cheap gates first: this runs per row on every window load.
	if payloadMeta == "" || !strings.Contains(payloadMeta, `"previewPatch"`) {
		return payloadMeta, false, false
	}
	if inlinePreviews && len(payloadMeta) <= InlinePreviewMaxBytes {
		return payloadMeta, false, true
	}

	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(payloadMeta), &top) != nil || top == nil {
		return payloadMeta, false, false
	}
	rawDiff, ok := top["inlineDiff"]
	if !ok {
		return payloadMeta, false, false
	}
	var diff map[string]json.RawMessage
	if json.Unmarshal(rawDiff, &diff) != nil || diff == nil {
		return payloadMeta, false, false
	}
	var files []json.RawMessage
	if json.Unmarshal(diff["files"], &files) != nil || len(files) == 0 {
		return payloadMeta, false, false
	}

	budget := 0
	if inlinePreviews {
		budget = InlinePreviewMaxBytes
	}
	elidedAny, keptAny, changed := false, false, false
	for i, rawFile := range files {
		var file map[string]json.RawMessage
		if json.Unmarshal(rawFile, &file) != nil || file == nil {
			continue
		}
		patch, ok := file["previewPatch"]
		if !ok || len(patch) <= 2 {
			continue
		}
		if len(patch) <= budget {
			// Spend the running budget across files rather than
			// stopping at the first overage: a break here would let
			// one large file starve every later small one, and the
			// reader would see a diff card that thins out for no
			// reason (remote-access.md §14, "a budget skips, it does
			// not break" — same shape as
			// highlightapp.capPatchSpanSeedBytes).
			budget -= len(patch)
			keptAny = true
			continue
		}
		delete(file, "previewPatch")
		file["previewElided"] = json.RawMessage(`true`)
		encoded, err := json.Marshal(file)
		if err != nil {
			keptAny = true
			continue
		}
		files[i] = encoded
		elidedAny, changed = true, true
	}
	if !changed {
		return payloadMeta, false, keptAny
	}

	encodedFiles, err := json.Marshal(files)
	if err != nil {
		return payloadMeta, false, true
	}
	diff["files"] = encodedFiles
	encodedDiff, err := json.Marshal(diff)
	if err != nil {
		return payloadMeta, false, true
	}
	top["inlineDiff"] = encodedDiff
	out, err := json.Marshal(top)
	if err != nil {
		return payloadMeta, false, true
	}
	return string(out), elidedAny, keptAny
}

// EncodedBytes estimates what one projected item costs on the wire.
// It is a sum of the variable-length fields plus a constant for the
// fixed keys, quoting and separators, not a marshal: the window
// backstop charges it once per row per load and a reflection encode
// there would cost more than the bytes it saves. Escaping is not
// modelled, so the estimate runs slightly low on text with quotes —
// the backstop is a ceiling on pathological rows, and being a few
// percent generous on them is the safe direction.
func EncodedBytes(item store.Item) int {
	const fixedOverhead = 220 // the always-present keys, braces and separators
	return fixedOverhead +
		len(item.ID) + len(item.ThreadID) + len(item.Kind) + len(item.Role) +
		len(item.Status) + len(item.Summary) + len(item.PayloadID) +
		len(item.PayloadKind) + len(item.PayloadMeta) + len(item.PayloadPreviewSpans) +
		len(item.InputPayloadID) + len(item.ParentID) + len(item.CompletionOf) +
		len(item.ToolName) + len(item.Decision) + len(item.Meta)
}
