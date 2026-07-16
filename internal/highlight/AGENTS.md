# internal/highlight/

Theme-independent syntax-highlight span metadata for the frontend's
diff cards, review pane, and markdown code blocks. Tree-sitter (CGO)
parses whole documents server-side and returns class ids over byte
ranges; the frontend maps ids to `syntax-<name>` CSS classes and slices
the text it already holds.

## Doctrine

**Spans are metadata, not markup.** A previous server-side highlighter
(goldmark+chroma, removed in `2ed0609f`) shipped rendered HTML and was
deleted because raw content is canonical. This package does not reverse
that decision: it returns the same kind of overlay metadata as
`PathRef[]` linkification — class ids over byte ranges, never HTML.
The frontend owns all DOM.

**Whole-document parsing is the point.** Both previous frontend Shiki
stacks tokenized per line, statelessly, which broke every multi-line
construct (Python docstrings, block comments, template literals). Every
entry point here parses a complete virtual document — for patches, each
hunk's reconstructed old/new sides — so grammar state carries across
lines by construction.

## Layout

| File | Role |
|---|---|
| `doc.go` | Package purpose + declined-optimization notes. |
| `registry.go` | `Lang` enum, extension map (mirrors what frontend `diffLanguage.ts` did), fence-name aliases. |
| `classids.go` | Class taxonomy (~28 coalesced families), capture-name → class coalescer. The taxonomy is the CSS contract — a one-way door; extend, don't renumber. |
| `encode.go` | Per-byte class buffer → `EncodedLine` run-length pairs `[byteLen, classId, ...]`. Nil runs = plain line (the common case). |
| `caps.go` | Input caps + parse timeout. The primary defense: a cgo parser crash is process-fatal, so pathological inputs are bounded out before reaching C. |
| `patch.go` | Unified-diff parser → per-hunk old/new virtual docs + patch-line refs. Response contract is patch-aligned: result line `i` ↔ patch text line `i`. |
| `jshash.go` | Frontend hash parity: fnv1a over UTF-16 code units (`FrontendContentKey`, `FrontendLineHashes`), byte-compatible with `frontend/src/lib/utils/fnv1a.ts`. Parity pinned by node-generated vectors in `jshash_test.go`. |
| `fencescan.go` | Streaming markdown fence scanner (`ScanFences`) for the highlight seed push. Deliberately stricter than marked (flush-left openers only) — divergence is fail-safe, see below. |
| `patchsplit.go` | `SplitPatchFiles`: multi-file unified diff → per-file (path, patch-text) seeds, mirroring the frontend's `parsePatchFiles` byte-for-byte. |
| `version.go` | `SchemaVersion()`: fingerprint over the class taxonomy + grammar/query set, exposed by the `HighlightSchemaVersion` RPC. Version-stamps every persisted span blob so spans computed under an old grammar are dropped, not misrendered. |

## Contracts

- **Degrade, never error.** Unknown language, over-cap input, parse
  timeout, malformed patch → plain spans (`Truncated` flagged where
  applicable). Highlighting failures must never break a diff render.
- **Absent lines are plain.** `Result.Lines` may be SHORTER than the
  input's line count (nil for unknown languages and oversized inputs);
  every renderer treats a missing index as a plain line. This is what
  keeps allocations bounded: no path allocates proportional to
  unbounded wire input (`MaxRequestBytes`, `maxResultLines`, and the
  per-patch `patchParseBudget` are the gates).
- **Wall-clock degradation is transient, size caps are permanent.** A
  result degraded by a failed parse or by the per-patch parse budget
  is marked `Incomplete` and never memoized (Cache skips it — the same
  input can succeed on an idle retry); a result degraded by a
  byte/line cap is deterministic and caches normally. `Incomplete`
  crosses the RPC boundary (`HighlightResult`), and the frontend
  caches apply the same rule: `codeSpanCache` skips it, the diff span
  cache retries it at a damped cadence. Parse timeouts go through the
  parser-level deadline, not `ParseWithOptions` — the v0.25 binding
  leaks its options payload on every call (see pool.go).
- **Byte offsets.** Run lengths are UTF-8 byte counts; the frontend
  slices its own copy of the text. The server never re-ships content.
- **Patch alignment.** `HighlightPatch` results index 1:1 with the
  patch's `\n`-split lines. Add/del spans cover the prefix-stripped
  body; context spans get a 1-byte plain pad (the frontend keeps the
  leading space on context lines). Meta/`@@`/`\`-marker lines are plain.
- **Per-hunk isolation.** Hunks parse as independent documents so a
  construct left open at the end of one hunk can't poison the next
  across the invisible gap.
- **Unmapped captures fail CI, render plain at runtime.**
  `captureFamily` returns ok=false for unknown names; the query-compile
  harness turns that into a loud test failure, the runtime path into
  `ClassNone`.

## Seed push (remote clients) and persisted spans

Clients don't pay a highlight RPC round trip for always-rendered
content: the app layer precomputes spans as the same seed wire shapes
and both pushes them live and persists them with history —
`app_highlight_seed.go` (streaming code fences → `highlight:seed`
events, plus settle-time `codeSpans` enrichment into `items.meta` via
the triage `SetCodeSpanEnricher` hook), `app_highlight_diff_seed.go`
(diff-payload persist tap → `payloads.preview_spans` / `payloads.spans`
columns, plus `highlight:diff_seed` events for remote clients). Cold
loads read the persisted blobs; live remote clients get the event
push; the RPC path covers every miss. Persisted blobs are stamped with
`SchemaVersion()` (`hv`) and content-addressed per fence/file, so a
stale blob — old grammar, edited content — is inert and the RPC
recomputes. Rules that live here:

- **Seeds still ship no content.** Code-fence seeds carry a cumulative
  per-line hash chain (`FrontendLineHashes`); the client verifies its
  own copy line by line and adopts spans only for the verified prefix.
  Diff seeds carry the frontend cache key (`path` +
  `FrontendContentKey(patch text)`); a key that doesn't match what the
  client computes is simply never looked up.
- **Seeds are complete, valid-UTF-8 results only.** Producers skip
  `Incomplete` results (a pre-inserted incomplete entry would sit out
  the client's damping window with nothing scheduled to retry it — the
  RPC path owns incomplete retries) and skip invalid UTF-8 sources:
  JSON transport maps each invalid byte to U+FFFD, so the client's
  hash/key would MATCH while the spans cover the original byte lengths
  — the one divergence that would misalign colors instead of missing
  the cache.
- **Divergence is fail-safe by construction.** `ScanFences` vs marked,
  `SplitPatchFiles` vs `parsePatchFiles`, hash parity — any drift makes
  a seed miss and the client falls back to the RPC path (today's
  behavior), never to misaligned colors. Keep parity tests
  (`jshash_test.go`, `patchsplit_test.go`, `fencescan_test.go`) in
  lockstep with the frontend sources they mirror.
- **Live parses don't pollute the cache.** `Cache.CodeTransient` serves
  the growing prefix of an open fence without memoizing it; only final
  content (`Cache.Code`, `Cache.Patch`) warms the shared cache, where a
  later RPC for the same content becomes a lookup.

## Extension points

- New language: add the `Lang` constant + names in `registry.go`, a
  `grammars/<lang>/` dir (binding + vendored queries + LICENSE +
  UPSTREAM), and golden fixtures. The query-compile harness picks it up
  automatically.
- New semantic class: append to `classids.go` (never renumber), add the
  `--syntax-*` CSS variable in `frontend/src/app.css` for both themes.
