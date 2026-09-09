# lib/markdown/

This is the first-party streaming Markdown parser and renderer. Fix parsing and
rendering defects here rather than post-processing output in chat wrappers. App
code imports only through `index.ts`; adding an export widens the public API.

## Streaming architecture

An assistant message is split at the last stable block boundary:

- The committed prefix uses `CompactBlocks.svelte` and `staticHtml.ts` to retain
  fixed-tag DOM for sealed blocks.
- The one volatile tail uses reactive `Block.svelte` components, with its final
  text leaf owned by `LiteralHost.svelte`.

Append work must scale with new bytes. Reuse happens in `parseBlocks.ts` at the
block level, `incrementalLex.ts` within an open block, and the compact renderer
at the DOM level.

`ProvenAppend` carries producer evidence that one source extends another. Do not
inspect a growing source with `startsWith`, prefix `slice`, or another operation
that can flatten V8 cons strings. Validate proofs with `matchesProvenAppend` and
fall back to a full parse when validation fails.

Completed block strings are materialized once at the parser boundary so token
substrings do not retain an entire message backing store. Do not materialize the
volatile block on every append.

## Package map

- `boundary/`: stable-prefix and volatile-tail splitting
- `parser/engine/`: the adapted marked lexer, grammar, tokens, and options
- `parser/extensions/`: app Markdown extensions
- `parser/parseBlocks*`: document-level block caching and source windows
- `parser/incrementalLex*`: within-block append reuse
- `parser/incompleteMarkdown*`: incomplete-construct completion
- `render/`: component and compact-static render paths
- `smoothing/`: pacing primitive used by the owning thread store

## URL and HTML boundary

Path-relative and protocol-relative links or images never render as raw anchors
or image sources. Render them only after `transformUrl` approval, or as a
non-navigable reference. Hosts that support path or preview actions claim those
tokens with parser extensions.

Embedded forge HTML is opt-in. The extension maps supported structural and
inline forms onto native tokens, then `render/htmlSanitize.ts` handles the
remaining allowlisted elements and attributes. Unknown HTML renders as escaped
text. Agent chat keeps HTML rendering disabled. See
[`remote-access-boundaries.md`](../../../../docs/specs/remote-access-boundaries.md).

Keep `staticHtml.ts` and `Element.svelte` behavior equivalent for every token and
URL class. The static path must bail out to a component island for component-
backed tokens such as Mermaid, including when highlight caches are warm.

## Host seams

- `LiteralHost` has one imperative owner. An app host may adopt it during
  streaming; visible text extends until a real divergence replaces it once.
- `CompactBlocks` preserves reference-identical prefix DOM and rebuilds only a
  rewritten suffix. It explicitly mounts and retires component islands.
- `data-streamdown-last-block` publishes the stable prefix's final token type.
- `sd-trim-first-block`, `sd-trim-last-block`, `sd-first-block`, task-item
  classes, and paragraph-adjacency markers replace broad structural selectors.
- `data-footnote-label` publishes labels only. Chat owns the definition registry
  and popup and pays the whole-document definition lex when opening it.
- Mermaid and math elements publish parser-owned source values. Code source
  remains the code element's text content.

Do not replace these seams with document-wide observers or structural CSS
selectors. Scope Markdown styles to Markdown-owned markup.

## Parser invariants

- A blockquote gets its own `Lexer`; shared instances leak inline queues and
  reference definitions across documents.
- A blockquote token's `raw` is exactly the consumed source prefix. Offset sums
  depend on it.
- Extension tokenizers check their opening character codes before context lookup
  or parsing. They run at every candidate position.
- Link rendering changes use inline extensions rather than a global `link`
  snippet, which disables compact static rendering for every link.
- Keep exact live source windows for append paths. Reclassification of omitted
  HTML or definitions may require the canonical source and move a boundary
  backward.

## Validation

Performance changes require differential tests against a fresh lex and full
render across varied chunk boundaries. Preserve sealed-token and DOM identity,
source offsets, CRLF behavior, and fallback correctness. Use workload tests for
byte and grammar-call budgets, subprocess heap tests for backing-store
retention, and browser tests for selection, DOM identity, and component-island
lifecycle.

`freezeReplay.manual.ts` is an operator-only replay under `pnpm test:manual`.
Its real-session corpus remains gitignored.

## Provenance

`render/` and `parser/extensions/` descend from `svelte-streamdown@3.1.2`;
`boundary/` descends from incremark; `parser/engine/` adapts marked 16.4.2's
lexer. Preserve their license files. These are maintained as first-party code,
not rebased vendor trees. When changing lexer grammar, compare current marked
fixes where useful while preserving this renderer's documented token shapes and
security properties.
