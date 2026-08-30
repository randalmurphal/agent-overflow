# lib/markdown/

The streaming markdown renderer. First-party source: fix parser and
render bugs HERE, never by post-processing in `chat/markdown/` or
`markdownEnhance.ts`.

The app imports through `index.ts` and nothing deeper. A symbol reaching
that barrel is a deliberate widening.

## The shape of a streamed message

One assistant message renders as TWO Streamdown roots, split at the last
stable block boundary (`boundary/`, the incremark detector):

- **committed prefix** — everything sealed. `parseIncompleteMarkdown ===
  false`, and with `compactStaticHtml` it renders through
  `CompactBlocks.svelte` as fixed-tag HTML over the parser cache's stable
  block array, not as a Svelte component tree.
- **volatile tail** — the one growing block. Reactive `Block.svelte`
  components, `parseIncompleteMarkdown === true`, its final text leaf
  handed to `LiteralHost.svelte`.

Everything else in this tree exists to make an APPEND cost O(new bytes)
rather than O(document) at each of three layers:

| layer | unit of reuse | entry point |
|---|---|---|
| document → blocks | the block raw | `parser/parseBlocks.ts` |
| block → tokens | the list item / table source row / open-fence line | `parser/incrementalLex.ts` |
| tokens → DOM | the sealed subtree (`===` prop equality) or the appended Text node | `render/CompactBlocks.svelte`, `render/literalHost.ts` |

`ProvenAppend` is what makes the first two possible. Establishing "this
input extends that one" by `startsWith` FLATTENS a V8 cons string, so
the prefix check alone copies the whole document per revealed word. The
producer of the delta mints the proof instead; a stale or fabricated
proof fails `matchesProvenAppend` and falls back to a full parse. Every
fast path in this tree falls back that way, so correctness never depends
on one engaging.

The smoother that drives reveal deltas is NOT here — it is
`stores/threadRevealSmoothers.ts`, behind the chokepoint in
`stores/threadStreamingReveal.svelte.ts`, and its one rule is
[stores/AGENTS.md § The reveal invariant](../stores/AGENTS.md#the-reveal-invariant).
`smoothing/PerItemSmoother.ts` is the pacing primitive it uses.

## Where things live

```
index.ts            the public barrel
parser/
  index.ts            internal barrel (re-exports only)
  engine/             the absorbed marked 16.4.2 lexer: Lexer, Tokenizer,
                      rules (grammar + our overrides), helpers, tokens,
                      options
  lexer.ts            extension registries, cached options,
                      lex / lexCapture /
                      lexFootnoteDefinitions / isKeptType
  provenAppend.ts     createProvenAppend, createMaterializedProvenAppend,
                      matchesProvenAppend, materializeString
  geometry.ts         shared append-safety predicates: scanFenceBody,
                      openFenceInfo, sealedLengthOf, tableAppendInfo,
                      tableTailUnsafe, paragraphAppendSafe
  parseBlocks.ts      parseBlocks, initialBlockToken, blockTokensOf
  parseBlocks.cache.ts        the raw/block arrays, the materialization
                              pass, updateTrailingBlockRecord
  incrementalLex.ts           incrementalLex + the open-fence fast path
  incrementalLex.cache.ts     per-Block state, trimBlock
  incrementalLex.merge.ts     mergeTrailingList / mergeTrailingTable /
                              reuseUnchangedTokens
  incompleteMarkdown.ts       the completer engine + registration order
  incompleteMarkdown.plugin.ts      Plugin contract + shared line scans
  incompleteMarkdown.context.ts     block contexts, fence sealing
  incompleteMarkdown.inline.ts      speculative inline emphasis (DISABLED)
  incompleteMarkdown.structural.ts  links, footnotes, math, dl, MDX
  extensions/         the 13 engine extensions (alert, list, table, math, …)
render/
  Streamdown.svelte   the root: props → context → blocks → one of two paths
  Block.svelte        one volatile block, reactive
  CompactBlocks.svelte + staticHtml.ts   the completed fixed-tag path
  LiteralHost.svelte + literalHost.ts    the trailing-leaf controller
  context.svelte.ts   the shared context (async settle, theme, snippets)
  elements/           Element, Link, Image, Math, Mermaid, Alert, …
boundary/             the block-boundary splitter (incremark, own LICENSE)
smoothing/            PerItemSmoother
```

## Security boundary

**A path-relative href or src NEVER renders a raw anchor or image.**

Upstream rendered any `/`-leading URL through a branch that bypassed
`transformUrl`. In an SPA host that anchor is a same-tab top-level
navigation onto the app origin: a 404 against the transport server for
`[x](/home/user/file.md)`, and for a crafted `[x](/design/...)` a
confirmed origin-isolation escape (agent-authored HTML served
same-origin could read the bootstrap token). `//host/x` counted as
"relative" too, giving protocol-relative navigation off-origin. The
`<img>` element had the identical bypass on `src`.

Both branches are removed. An anchor renders only for a
`transformUrl`-approved href, an `<img>` only for a
`transformUrl`-approved src, and everything else falls to the
non-navigable reference span. A host that wants path-shaped hrefs to DO
something rewrites them during parsing — `utils/pathLinkExtension.ts`
turns them into nonce'd `agent-overflow:open` editor links.

Cited by
[`remote-access-boundaries.md`](../../../../docs/specs/remote-access-boundaries.md).
Pinned by `ChatMarkdown.test.ts` ("never renders a raw same-origin
anchor…" / "…img…").

## Host seams

Five places where this tree deliberately publishes something for the app
rather than rendering it, plus the one it hands over entirely. Each was
a measured cost, not a preference; drop one only with the measurement
that says it no longer pays.

- **The active trailing literal leaf has ONE imperative owner.**
  `LiteralHost.svelte` renders an EMPTY span and attaches
  `literalHost.ts`'s controller, which is the single writer of that
  element's children. The renderer publishes the parser's authoritative
  leaf text INTO it; an app-side owner may `adopt` the element and take
  over while it streams. Two writers on one visible text run was a
  defect, not a division of labour — the app deleted its appended
  siblings in one task while Svelte re-extended its own parser-owned
  node in a later one, and WebView2 presented the rollback as a settle
  flicker. The rule both writers are held to is ownership, not timing:
  the visible string only ever EXTENDS, until a genuine divergence
  replaces it in a single `replaceChildren` mutation.
- **Completed blocks render as fixed-tag HTML, not components.**
  A 7.3K-character rich answer held >10K non-element control-flow nodes
  in Blink once settled, multiplied across every visible pane.
  `CompactBlocks` owns the completed prefix, keeps reference-identical
  prefix DOM, appends HTML for new blocks and rebuilds only a rewritten
  suffix. It mounts a real `Block` island when serialization rejects a
  custom or component-backed token, and unmounts it explicitly.
- **`data-streamdown-last-block`** carries the last rendered block's
  token type. The committed/volatile seam needs "does the prefix end in
  a paragraph"; a CSS `:has()` over the committed subtree answered it,
  but every streamed descendant insertion then invalidated the ancestor
  selector across all visible panes.
- **`sd-trim-first-block` / `sd-trim-last-block`** carry message-edge
  margin trimming. Structural `:first-child` / `:last-child` made every
  nested sibling change — including highlighted-span replacement —
  schedule an invalidation set over all prior `md-blk` elements: ~4,600
  elements and 32ms per style pass on a 65K four-pane run. The HOST
  states which edges this root owns; this tree moves the class.
- **`sd-first-block`, the task-item class, parser-derived paragraph
  adjacency.** Same disease, three ordinary Markdown rules:
  `p:first-child`, `li:has(> input)` and `p + p` all had document-wide
  invalidation keys. Volatile paragraphs keep a class-keyed sibling rule
  because that tree is the bounded unstable tail.
- **`data-footnote-label`** is the footnote seam. A `[^1]: body`
  definition renders nothing here and each Block lexes its string in its
  own Lexer, so a ref and its definition never meet and `token.content`
  is empty for every real document. The chip stamps the label plus an
  unconditional `aria-haspopup="dialog"` and stops — no handler, no
  popup, no state. `lexFootnoteDefinitions` exports one whole-document
  lex; `chat/markdown/footnoteDefinitions.ts` owns the registry and
  memo, and `chat/FootnotePopoverHost.svelte` renders the body on one
  app-level popup. The lex is paid on the click, never during render.

`data-mermaid-source` and `data-math-source` follow the same pattern for
values only the parser has. Code uses a source-free `data-code-source=""`
marker: `<code>.textContent` owns the source.

## Landmines

- **Token raws are V8 substrings of the parser input.** A
  long-lived block array therefore retains one whole historical document
  per checkpoint that introduced a block.
  `updateParseBlockStringMaterialization` copies each completed block
  once, at the parser boundary. The compact renderer enables it; the
  volatile renderer must NOT (its final block is replaced on every
  append, so copying a growing block per reveal is O(n²)). Tripwire:
  `parseBlockRetention.test.ts`, a whole-heap delta in a subprocess.
- **Any prefix inspection of the streaming source flattens it.** That is
  what `ProvenAppend` exists to avoid; a new fast path takes the proof,
  never `startsWith` / `slice` on the full input.
- **A blockquote is lexed by a blockquote-SCOPED Lexer.** A module-level
  one accumulated an undrained `inlineQueue` entry per quoted paragraph
  for the life of the page (16,511 over one corpus) and carried
  reference definitions between documents.
- **A blockquote token's `raw` is the consumed prefix of the source.**
  marked rebuilt it by re-joining walked lines and spliced inner
  marker-stripped raws back in, so `raw` came back holding bytes the
  source never had at that offset — and, in the list-continuation
  branch, one byte more than the block rule even matched. Nothing
  crashed, because marked only reads `raw.length` — but a block token's
  `raw` naming its consumed bytes is the contract every offset sum in
  this tree tests against. `engine/Tokenizer.ts#blockquote` now returns
  `src.slice(0, …)` and the shim `marked-alert` carried is gone.
- **The extension tokenizers gate on a character code first.** The
  lexer invokes every registered tokenizer at every candidate position;
  the inline footnote one called `getContext` before checking for `[^`, so
  ordinary prose paid a thrown-and-caught Svelte lifecycle error per
  inline token. An extension-heavy 20,000-parse benchmark fell 1.98s →
  0.46s when the gates went in.
- **A stylesheet from this tree must not reach a component that has
  never rendered markdown.** An unscoped `:global([data-expanded='true'])`
  pinned the workflows run map's expanded wave row `position: fixed`
  over the whole viewport at `z-index: 2147483647`, swallowing every
  click underneath.

## Tests

Behavioral changes need a differential, not a snapshot: these paths are
"identical output, less work" by construction.

| file | covers |
|---|---|
| `incrementalLex.test.ts` | byte-equivalence of every fast path against a fresh `lex`, across chunkings; sealed-token reference identity; `lastPath` breadcrumbs; perf contracts |
| `parseBlocksDifferential.test.ts` | fresh-parse and full-render parity over deterministic mixed-Markdown streams, source-only list/table geometry, CRLF, zero grammar calls for impossible extension starts |
| `parseBlocksWorkload.test.ts` | the real active-pane path and its byte budgets |
| `parseBlockRetention.test.ts` | the V8 backing-store tripwire (subprocess heap delta) |
| `alertBlockquote.test.ts` | blockquote Lexer scoping + `raw` fidelity |
| `listMarkerCode.test.ts` | aligned list markers are not indented code, streamed vs settled |
| `streamingMarkdownPipeline.test.ts` | splitter → parseBlocks → incrementalLex end to end |
| `boundary/*.test.ts` | the block-boundary splitter |
| `chat/markdown/streamdownSingleVolatileBlock.test.ts` | the isolated-volatile-tail bypass of the document parse |
| `chat/markdown/streamingAssistantLiteralOwner.test.ts` | literal-host ownership transitions |
| `chat/markdown/streamdownTheme.test.ts` | the flat theme table is complete — it derives the slot roster from the render path's own source |
| `chat/ChatMarkdown.test.ts` | link/image URL policy, backslash escapes, link titles, the footnote popup |
| `chat/ChatMarkdown.compactStatic*.test.ts`, `.domBudget`, `.codeSpans*` | the compact completed path, its node budget and async island retirement |
| `chat/ChatMarkdown.boundarySpacing.*`, `styleInvalidation.test.ts` | the `sd-*` marker seams |
| `chat/ChatMarkdown.directReveal*.test.ts` | the extend-only mutation invariant, selection identity |
| `chat/AssistantMessage.test.ts` | the completer battery (fence seal, setext guard, `$`-prose, sub/sup ranges) |
| `chat/footnotePopover.browser.test.ts` | the popup on the real popup layer |
| `chat/streamingIncrementalReuse.browser.test.ts` | DOM identity of sealed `<li>`/`<tr>` through a real drain |

`freezeReplay.manual.ts` replays a recorded session corpus through the
production path and WEDGES on a finding. Operator-run only
(`pnpm test:manual`); its corpus is gitignored and must never be
committed.

## Provenance

`render/` and `parser/extensions/` descend from `svelte-streamdown@3.1.2`
(MIT, in `LICENSE`); `boundary/` from incremark (MIT, its own LICENSE).
Both upstreams are dormant and there is no rebase to preserve — this is
ordinary first-party code now, so fix it like any other file. The
incremental parser, the literal host, `CompactBlocks`, `staticHtml` and
every test beside them are original.

`parser/engine/` is marked 16.4.2's lexing half (MIT, in `LICENSE`),
absorbed in W4 of
[`markdown-first-party.md`](../../../../docs/specs/markdown-first-party.md).
The `marked` package, its pnpm patch and its 16.4.2 pin are gone. The
Parser/Renderer/Hooks/`marked()` surface was never reachable from here
and is not absorbed. Divergences from upstream are commented at their
sites, in three groups: the allocation-free extension dispatch (one
receiver per Lexer, indexed loops — formerly the pnpm patch), the app's
own rule overrides (`~~`-only strikethrough, no mailto autolinking,
homogeneous leading runs in the GFM inline `text` rule) and the
blockquote `raw` fix above. The 17.x/18.x ReDoS and quadratic-backtracking
fixes are ported by semantics; their token-shape changes deliberately are
not, so output stays 16.4.2-identical. Fix bugs there like any other file
— but a fix that upstream also made is worth diffing against, because the
grammar tables are still recognisably theirs.
