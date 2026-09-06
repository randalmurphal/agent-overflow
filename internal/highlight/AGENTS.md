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
`PathRef[]` linkification. Class ids over byte ranges, never HTML.
The frontend owns all DOM.

**Whole-document parsing is the point.** Both previous frontend Shiki
stacks tokenized per line, statelessly, which broke every multi-line
construct (Python docstrings, block comments, template literals). Every
entry point here parses a complete virtual document (for patches, each
hunk's reconstructed old/new sides) so grammar state carries across
lines by construction.

## Contracts

- **Degrade, never error.** Unknown language, over-cap input, parse
  timeout, malformed patch → plain spans (`Truncated` flagged where
  applicable). Highlighting failures must never break a diff render.
- **Input caps are a crash defense, not a perf tweak.** A cgo parser
  crash is process-fatal, so `caps.go` bounds pathological inputs out
  before they reach C.
- **The class taxonomy in `classids.go` is the CSS contract**, a one-way
  door shared with `frontend/src/app.css`. Extend it, never renumber.
- **Absent lines are plain.** `Result.Lines` may be SHORTER than the
  input's line count (nil for unknown languages and oversized inputs);
  every renderer treats a missing index as a plain line. This is what
  keeps allocations bounded: no path allocates proportional to
  unbounded wire input (`MaxRequestBytes`, `maxResultLines`, and the
  per-patch `patchParseBudget` are the gates).
- **Wall-clock degradation is transient, size caps are permanent.** A
  result degraded by a failed parse or by the per-patch parse budget
  is marked `Incomplete` and never memoized (Cache skips it, since the
  same input can succeed on an idle retry); a result degraded by a
  byte/line cap is deterministic and caches normally. `Incomplete`
  crosses the RPC boundary (`HighlightResult`), and the frontend
  caches apply the same rule: `codeSpanCache` skips it, the diff span
  cache retries it at a damped cadence. Parse timeouts go through the
  parser-level deadline, not `ParseWithOptions`. The v0.25 binding
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
- **Primed docs splice BOTH sides of the hunk.** `PatchWithContext`
  builds prefix + hunk + suffix from the resolved file content. The
  suffix is load-bearing, not garnish: a hunk inside a raw-text
  element (svelte/html `<script>`) paints fully plain without the
  closing tag, because the grammar never emits the node the language
  injection anchors on. Changing this construction changes span
  output for identical inputs. Bump `patchDocStrategyVersion`
  (version.go) so persisted blobs computed under the old strategy
  retire instead of pinning stale colors.
- **Unmapped captures fail CI, render plain at runtime.**
  `captureFamily` returns ok=false for unknown names; the query-compile
  harness turns that into a loud test failure, the runtime path into
  `ClassNone`.

## Seed push and persisted spans

Clients don't pay a highlight RPC round trip for always-rendered
content: the app layer precomputes spans as the same seed wire shapes
and both pushes them live and persists them with history —
`internal/highlightapp/seed.go` (streaming code fences → `highlight:seed`
events for remote clients, plus settle-time `codeSpans` enrichment
into `items.meta` via the triage `SetCodeSpanEnricher` hook),
`internal/highlightapp/diff.go` (diff-payload persist tap →
`payloads.preview_spans` / `payloads.spans` columns, plus
`highlight:diff_seed` events for every client). Diff seeds go to
loopback clients too because persist time is the one moment the
workspace file still matches the patch: the producer primes each
file's parse with real file content when
`PatchMatchesContent(patch, content)` verifies the match, marks those
seeds `Primed`, and the frontend cache upgrades unprimed entries in
place (never the reverse). That is quality no loopback RPC recompute can
reach later. Cold loads read the persisted blobs; live clients get
the event push; the RPC path covers every miss. Persisted blobs are
stamped with
`SchemaVersion()` (`hv`) and content-addressed per fence/file, so a
stale blob (old grammar, edited content) is inert and the RPC
recomputes. Rules that live here:

- **Seeds still ship no content.** Code-fence seeds carry a cumulative
  per-line hash chain (`FrontendLineHashes`); the client verifies its
  own copy line by line and adopts spans only for the verified prefix.
  Diff seeds carry the frontend cache key (`path` +
  `FrontendContentKey(patch text)`); a key that doesn't match what the
  client computes is simply never looked up.
- **Seeds are complete, valid-UTF-8 results only.** Producers skip
  `Incomplete` results (a pre-inserted incomplete entry would sit out
  the client's damping window with nothing scheduled to retry it, since
  the RPC path owns incomplete retries) and skip invalid UTF-8 sources:
  JSON transport maps each invalid byte to U+FFFD, so the client's
  hash/key would MATCH while the spans cover the original byte lengths,
  the one divergence that would misalign colors instead of missing
  the cache.
- **Divergence is fail-safe by construction.** `ScanFences` vs marked,
  `SplitPatchFiles` vs `parsePatchFiles`, hash parity. Any drift makes
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
- Configuration and build formats include TOML, INI, HCL/Terraform,
  Dockerfile/Containerfile, Make, XML, and PowerShell. Detection owns both
  extensions and conventional filenames, accepts Windows and Unix separators,
  and leaves ambiguous `.conf` files plain. INI covers `.gitconfig` and
  `.editorconfig`; it does not promise every dialect's extensions. JSON-form
  Terraform (`.tfvars.json`) remains JSON. Pinned grammar/query additions
  automatically change `SchemaVersion`; never hand-bump it or reuse spans
  from another schema. Code and contextual-patch fixtures verify multiline
  state and UTF-8 alignment alongside the shared parser/query/cap gates.
- New semantic class: append to `classids.go` (never renumber), add the
  `--syntax-*` CSS variable in `frontend/src/app.css` for both themes.
