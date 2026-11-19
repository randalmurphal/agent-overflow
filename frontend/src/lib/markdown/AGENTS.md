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

Path-relative links or images never render as raw anchors or image sources.
Render them only after `transformUrl` approval, or as a non-navigable reference.
`//host/x` resolves to `https:`, never to the page origin. Hosts that support
path, image or preview actions claim those tokens with parser extensions.

The `*` wildcard admits every scheme except the deny-list in
`render/elements/urlSchemes.ts` and `file:`; `internal/externalurl` mirrors the
list and a Go test compares the two. `classifyLinkHref` is the one decision for
anchor, untagged reference, or tagged blocked span; both renderers call it.

Embedded forge HTML is opt-in. The extension maps supported structural and
inline forms onto native tokens, then `render/htmlSanitize.ts` handles the
remaining allowlisted elements and attributes. Its output holds only
allowlisted elements, allowlisted attributes and text. HTML off the allowlist
renders as a forge renders it: the element's text shows without the element,
and a script, style or svg shows nothing. On a GitHub surface GFM's disallowed
tags (`<textarea>`, `<iframe>`) show as their source text. Agent chat keeps
HTML rendering disabled. An HTML block that leaves a
tracked container open until a later HTML block is one `htmlRegion` token
(`parser/extensions/htmlRegion.ts`). Its HTML is sanitized and parsed once
around a nonce-marked placeholder per Markdown island, as a forge sanitizes a
whole document; `HtmlRegion.svelte` renders island-free HTML through the same
`html` snippet as an html token. `markedEmbeddedInlineHtml` is
exported on its own so an app extension that must claim an `<img>` first
reuses this attribute parsing instead of re-implementing it. Media inside a
wrapper the extension does not claim reaches the sanitizer's `claimMedia`
option: a host returning a claim gets the element without its `src`, marked
with `data-markdown-media-claim`, and owns the injection through the `html`
snippet. See
[`remote-access-boundaries.md`](../../../../docs/specs/remote-access-boundaries.md).

A theme-specific image (a `<picture>` color-scheme source, a
`#gh-*-mode-only` fragment) paints the src the reader's scheme selects:
the renderer's `colorScheme` prop and the sanitizer's `colorScheme` option,
read only where such an image is, so a theme change repaints those images
and rebuilds no block. Every candidate passes the same URL gate and host
claim as the image's own src (`parser/extensions/themedImages.ts`).

Keep `staticHtml.ts` and `Element.svelte` behavior equivalent for every token and
URL class. The static path must bail out to a component island for component-
backed tokens such as Mermaid, including when highlight caches are warm.

## Host seams

- `LiteralHost` has one imperative owner. An app host may adopt it during
  streaming; visible text extends until a real divergence replaces it once.
- `CompactBlocks` preserves reference-identical prefix DOM and rebuilds only a
  rewritten suffix. It explicitly mounts and retires component islands. A
  changed extension list rebuilds every block unless each changed extension
  implements `tokenizesAlike`; then only the blocks it does not claim rebuild.
- `data-streamdown-last-block` publishes the stable prefix's final token type.
- `sd-trim-first-block`, `sd-trim-last-block`, `sd-first-block`, task-item
  classes, and paragraph-adjacency markers replace broad structural selectors.
- `sd-codespan-short` marks inline code short enough to stay on one line.
  Every emitter of an inline `<code>` calls `render/codespan.ts#codespanClass`.
- `data-footnote-label` publishes labels only. Chat owns the definition registry
  and popup and pays the whole-document definition lex when opening it.
- A `DocumentContext` (`parser/documentContext.ts`) folds per-block facts
  (reference definitions; on a forge, footnote numbers, heading anchors and
  GitLab's table of contents) and hands each block a `DocumentScope`. A
  block's lex records the keys it read; a renderer rebuilds kept output only
  when one resolves differently. Forge anchors are `data-md-anchor` and
  same-document links `data-md-fragment`, never `id` or a navigating href;
  chat resolves them inside the clicked `.markdown-body`.
- Mermaid and math elements publish parser-owned source values. Code source
  remains the code element's text content.

Do not replace these seams with document-wide observers or structural CSS
selectors. Scope Markdown styles to Markdown-owned markup.

## Parser invariants

- The alert extension measures a blockquote with a throwaway `Lexer` and lexes
  its body through the active one. A shared measuring instance leaks inline
  queues and reference definitions across documents.
- A tokenizer that builds a nested block token outside the block loop asks
  `Lexer.blockExtensionToken` first, so the token is the one the loop would
  produce there.
- Link text holds no link: a tokenizer that builds a link checks
  `lexer.state.inLinkText`, and one that lexes link text sets it. A raw
  `<a>` tag changes `inLink` only.
- A destination or title is resolved by the engine's `linkText` option, which
  an extension sets. Build link tokens from `tokenizer.link` so every host
  claim reads a destination the same way.
- A block extension that can claim a run opening in a sealed block (a wrapper
  closed by a later append) sets `reopensSealedBlocks`, which disables
  `parseBlocks` append reuse.
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
