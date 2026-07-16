# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `pnpm run check` — Svelte + TypeScript type check. Must pass.
- `pnpm run build` — production build. Must pass.
- `pnpm test` — Vitest unit tests.

## Layout

- `src/lib/stores/` — runes-based reactive stores. `thread.svelte.ts`
  owns the per-thread `ThreadPane` factory, composed from its
  `thread*` sub-factory modules (`threadChannelState.svelte.ts`,
  `threadTimelineWindow.svelte.ts`, `threadStreamingReveal.svelte.ts`,
  …); `events.ts` is a thin composition root that fans backend events
  out to the `events*` domain modules (`eventsItemStream.ts`,
  `eventsProvider.ts`, `eventsDiscussion.ts`, …); `bindings.ts` wraps
  generated Wails calls.
- `src/lib/components/panes/` — pane host/layout surfaces. This is the
  only place that should translate layout items into mounted chat panes.
- `src/lib/components/chat/` — timeline rendering. Kind-based
  discrimination; no role/content matching. See its local guide before
  editing rows, virtualized scrolling, markdown, or review/companion
  pane affordances.
- `src/lib/components/composer/` — message composer, mode / effort /
  model pickers.
- `src/lib/components/sidebar/` — projects + thread list.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Pickers compose these rather than rolling their own
  positioning, focus-trap, or keyboard behavior.
- `src/lib/components/{design,discussion,git,palette,settings,terminal,usage,shared}/` —
  per-feature component groups.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `src/lib/transport/` — WebSocket client + `@wailsio/runtime` shim.
  Feature code should go through `stores/bindings.ts`, not this package.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.

## State Boundaries

`ThreadPane` is the sole owner of per-thread runtime UI state: visible
items, streaming flags, approvals, design artifacts, channel messages,
token usage, checkpoint bookkeeping, terminal placement, and scroll
controller registration. Companion pane layout/open state lives in the
pane-layout/companion stores. Do not add a parallel streaming or timeline
state slice next to it.

Pane layout and pane runtime state are separate. Layout stores own
placement/order/min-size metadata; `ThreadPane` owns the chat/discussion
runtime state mounted into that slot. Command palette actions must resolve
against an explicit target pane because enablement can change while the
palette is open.

The visible-thread memory budget is load-bearing. Heavy payloads (diffs,
command output, thinking, attachments) load on demand through bindings.
Thread switch is a bounded-window load or cache restore, not a full-history
hydrate. Settled subagent children are evicted from pane memory into a
per-anchor fold (`utils/subagentFold.ts`) and re-hydrate on card
expansion; see "Live Window Bounds" in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).

## Thread Switch And Scroll

The durable contracts for cache restore, tail-only initial load, lazy
older paging, scroll intent, the windowing engine, and scroll-regression
diagnostics live in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).
Read that before touching:

- `src/lib/stores/thread.svelte.ts`
- `src/lib/stores/threadItemCache.ts`
- `src/lib/stores/threadScrollSnapshots.ts`
- `src/lib/components/chat/MessageTimeline.svelte`
- `src/lib/components/chat/timeline{Restore,SizePriors,WindowAnchor,RowProjection}.svelte.ts`
  and `timeline{Paging,Diagnostics,RowUiPrune}.ts` (the scroll-session
  modules extracted from MessageTimeline)
- `src/lib/components/virtual/TimelineVirtualizer.svelte`
- `src/lib/components/discussion/ChannelView.svelte`
- `src/lib/utils/scroll/` (`index.svelte.ts` controller + resolver/intent/spring/observers)
- `src/lib/utils/virtual/` (windowing engine + per-thread size priors)

Short version: `MessageTimeline` owns the scroll container, the bespoke
virtualizer (`TimelineVirtualizer.svelte` over the pure engine in
`utils/virtual/`) owns row geometry and never writes `scrollTop`, and
the scroll controller (`utils/scroll/`) owns scroll intent and every
programmatic `scrollTop` write — inside the package the pure resolver
decides, the controller's `writeScrollTop` chokepoint writes. The
virtualizer's `scrollToIndex`, compensation observations, and
content-geometry samples all route through the controller
(`applyScrollTarget` / `applyEngineCompensation` /
`deliverContentGeometry` — chat runs no contentEl ResizeObserver);
`scrollToIndex` is instant-only by design.

## Rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend renders them as viewport-local
projections. Do not add server-rendered chat HTML or a global DOM
observer.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte` and `svelte-streamdown`. Path linkification happens
inside marked parsing using server-validated `PathRef[]` metadata and a
per-page-load nonce; click/copy behavior is delegated by
`markdownEnhance.ts`.

ANSI-like payloads render through `AnsiText.svelte`, which diffs into a
stable `<pre>` with Idiomorph so selection survives streaming updates.

## Anti-Patterns

- Do NOT create legacy stores. Runes only: `$state`, `$derived`,
  `$effect`, `$props`. No `export let`, no `$:`.
- Do NOT discriminate timeline items by role or content substring.
  Discriminate via `kind`.
- Do NOT re-order items during render. Upsert by `(turnIndex, itemIndex)`
  and let the store stay sorted.
- Do NOT implement count-based slicing for virtualization. Heavy content
  is expand-to-load, not preload.
- Do NOT stretch a `.svelte` file past roughly 300 lines when a clear
  component split exists.
- Do NOT put business logic in templates. Derive in `<script>`, render in
  the template.
- Do NOT call `window.runtime` directly. Use `stores/bindings.ts`.
- Do NOT preload heavy payloads.
- Do NOT add visible in-app explanatory text for internal mechanics,
  shortcuts, or implementation details.

## Testing

Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
Component behavior: add a component test when changing rendering or
interaction. Scroll behavior has dedicated coverage in
`src/lib/utils/scroll/index.svelte.test.ts` (controller
choreography), `src/lib/utils/scroll/resolver.test.ts` (the pure
decision core, exhaustive over its state × observation matrix), and
`src/lib/components/chat/scroll.test.ts`.

`pnpm run check` and `pnpm run build` are blockers. `pnpm test` is the
frontend unit-test gate.

## Vendor Patches

`patches/` holds pnpm patches, keyed to exact versions in
`pnpm-workspace.yaml` (`patchedDependencies`) — a version bump without
re-rolling the patch fails `pnpm install`. Never edit `node_modules`
directly; packages are hardlinked from the pnpm store, so direct edits
corrupt every project on the machine. Use
`pnpm patch <pkg>@<version> --edit-dir <dir>` + `pnpm patch-commit`.

- `svelte@5.56.3.patch` — three hunks with different drop rules:
  1. **zombie-mint fix** — reactivity leak where deriveds read during
     component init are force-connected and never released (upstream
     [sveltejs/svelte#18420](https://github.com/sveltejs/svelte/issues/18420)).
     Drop when `src/test/integration/svelte-patch-zombie-leak.test.ts`
     passes on an unpatched release.
  2. **ownerless-roots** — `$effect.root` no longer inherits the
     creating component's context/parent, so store-level roots
     (threadRowUiState's expansion registry) don't pin dead row
     instances. Deliberate divergence, no upstream issue — carry it
     forward and re-evaluate on every version bump. Regression suite:
     `svelte-patch-ownerless-roots.test.ts`.
  3. **zombie-mint probe** — diagnostic tripwire (receiver:
     `src/lib/utils/zombieMintProbe.ts`) that fires if a future svelte
     re-introduces the hunk-1 shape. Keep while hunk 1 exists; drop
     alongside it.
- `svelte-streamdown@3.1.2.patch` — markdown-pipeline fixes, grouped by
  concern. Behavior is held across version bumps: re-roll by
  `git apply --reject`-ing the prior patch into a clean `pnpm patch`
  edit-dir, hand-merging only the rejected hunks, then diffing the
  in-flight-completion battery old-vs-new to prove the inline path is
  byte-identical. Pieces to preserve on the next bump:
  1. **`$`-prose guard** (`marked/marked-math.js`,
     `singleDollarLooksLikeProse`) — a bare single `$` only opens inline
     math when its closer is not an identifier char and its content holds
     no backtick; otherwise agent prose like `$ref … $foo` renders as
     serif KaTeX. Regression: `components/chat/AssistantMessage.test.ts`.
  2. **lexer-rule overrides** (`marked/index.js`, top-of-file) — require
     `~~` for strikethrough (single `~` false-positives on `~240MB`),
     disable mailto autolinking (`composer@0.7s` → bogus `mailto:`), and
     split the GFM text rule's leading ``[`~]+`` run so a `~` cannot
     swallow an adjacent backtick. Composes with upstream's
     bottom-of-file options cache + incremental `parseBlocks`.
  3. **deferred-typesetting hosts** (`Elements/{Math,Mermaid}`,
     `context`, `Streamdown`) — `registerAsyncResource` /
     `pendingAsyncCount` / `onsettled` let a Streamdown signal when its
     async work (katex, mermaid, backend highlight spans) has settled,
     so the committed-prefix vs volatile-tail two-instance split in
     `ChatMarkdown.svelte` defers math/mermaid typesetting off the
     streaming tail. Our own `StreamdownCodeHost` participates through
     the same context hooks (no `Code.svelte` hunk needed — the
     library's shiki Code component is unused and tree-shaken).
     Orthogonal to upstream's parse cache — they stack.
  4. **completion-disable** (`utils/parse-incomplete-markdown.js`) — a
     trailing `.filter` drops the 10 inline emphasis/code/math
     speculative completers (they mis-close on lone delimiters
     mid-stream); the structural completers (links, citations,
     footnotes, fences, MDX) keep upstream's behavior. Re-enabling the
     safe ones (bold/italic/strike) is a separate follow-up. The
     isWithinInlineCode guards that used to patch the dropped
     completers were removed as dead code when the patch was re-rolled
     onto 3.1.2; recover both from git history if the re-enable
     follow-up happens.
  5. **subscript range-guard** (`marked/marked-subsup.js`) — `subRule`'s
     closing `~` carries a `(?!\d)` lookahead so approximate-range prose
     like `~5~10` / `~50~100` no longer tokenizes its low bound as
     `<sub>5</sub>`. Legit subscript (`H~2~O`, closing `~` before a
     letter) is unaffected. `supRule` (`^N^M`) is deliberately left
     unguarded — carets are rare in agent prose and none was reported;
     revisit if `^5^10`-style superscript false-positives surface.
     Regression: `AssistantMessage.test.ts`. Both sub/sup rules also
     exclude backticks from their content (code spans bind tighter, so
     a delimiter inside an inline code span can no longer open sub/sup
     across it).
  6. **setext-dash-guard** (`utils/parse-incomplete-markdown.js`,
     `stripDanglingSetextUnderline`) — mid-stream a nested bullet's marker
     arrives a chunk before its text, so the volatile tail momentarily ends
     in `…text:\n  -`; CommonMark reads that lone `-`/`=` run under a
     non-blank line as a Setext underline and balloons the line above to
     `<h2>`/`<h1>` for one chunk (font + margin + re-wrap), collapsing back
     the next chunk. The guard drops a trailing lone `^[ \t]*[-=]+[ \t]*$`
     line when the line above is non-blank, applied AFTER
     `defaultParser.parse` so an open fence is sealed first (a `-` inside it
     is no longer the last line). Streaming volatile-tail only: prefix and
     settled instances pass `parseIncompleteMarkdown === false`, so a
     *settled* Setext heading still renders (a streamed one defers one
     chunk — rare; agent prose uses ATX `#`). No upstream issue filed —
     a general svelte-streamdown streaming bug, candidate to upstream. Full
     rationale + the blank-above safety net are in the source comment.
     Regression: `AssistantMessage.test.ts`.
  7. **split-instance parse bypass** (`Block.svelte`) — honors
     `parseIncompleteMarkdown === false` from props; upstream types the
     prop but never reads it in Block, so the committed-prefix and
     settled instances in `ChatMarkdown.svelte` couldn't opt out of
     speculative completion without this. Trivial upstream-PR candidate;
     drop when upstream honors the prop.
  8. **block-math flexible form** (`marked/marked-math.js`, `blockRule`
     alt 3) — accepts `$$ CONTENT $$` where the content opens with a
     space (not a newline) and contains internal newlines, e.g.
     LLM-emitted matrices (`$$ \begin{pmatrix}…`). Without it those
     blocks fell through to paragraph rendering mid-stream ("math
     starts to render then turns back into raw markdown"). Closing `$$`
     must be followed by newline/end-of-string so adjacent same-line
     inline `$$X$$` pairs still take the single-line alternative.
  9. **typeset caches** (`Elements/Math.svelte`,
     `Elements/Mermaid.svelte`) — module-level KaTeX HTML cache
     (deterministic output, LRU cap 500); Mermaid SVG cache keyed on
     `(theme, sanitized source)` with a per-insertion uniqueId rewrite
     so two in-DOM instances of the same diagram can't collide on
     document-scoped `url(#…)`/`xlink:href` ids (LRU cap 100). Both
     exist because the committed-prefix/volatile-tail split remounts
     each settled block once — the caches make that migration free.
     (Code blocks get the same treatment from our own
     `markdown/codeSpanCache.ts`, outside the patch.) Perf-only: each
     can be dropped independently if upstream grows an equivalent.
  10. **relative-reference links** (`Elements/Link.svelte`) — the
      blocked-link branch drops its " [blocked]" suffix for schemeless
      relative references (`docs/guide.md`, `../x`, `#frag` — common
      repo-relative links in PR/issue bodies). They aren't navigable
      here but they aren't *blocked URLs* either; the text renders
      muted with the href as hover title. Disallowed absolute URLs
      (`javascript:` etc.) and network-path refs (`//host/x`) keep the
      tag. Regression: `ChatMarkdown.test.ts`. Upstream-PR candidate.
  11. **fence-seal fidelity** (`utils/parse-incomplete-markdown.js`,
      `contextManager`) — sealing a streamed open fence now replicates
      the opener's leading prefix (list indentation, blockquote `>`
      markers), fence char, and run length instead of always appending
      a flush-left ` ``` `. A flush-left closer under a list-indented
      fence is not a closer per CommonMark: it terminates the list and
      opens a NEW top-level fence, which rendered as a persistent
      phantom empty code-block container under the streaming one,
      vanishing with a layout snap when the real closer arrived. Same
      shape for blockquote-nested fences; `~~~` fences were sealed with
      a mismatched ` ``` `. The close toggle is now char/length-aware
      (a bare ``` inside a ```` fence is content, matching marked), and
      the seal drops a trailing half-streamed closer (lone `` ` ``/
      ` `` ` line) so the close moment doesn't grow-then-shrink by a
      line. Streaming volatile-tail only. Upstream-PR candidate.
      Regression: `AssistantMessage.test.ts` (fence-seal battery).

## References

- [`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md) —
  chat/discussion scroll architecture and diagnostics.
- [`docs/architecture/data-flow.md`](../docs/architecture/data-flow.md) —
  provider → triage → store → frontend pipeline.
- [`docs/architecture/how-to.md`](../docs/architecture/how-to.md) —
  extension playbooks.
- [`docs/references/spike-policy.md`](../docs/references/spike-policy.md) —
  when Wails or provider behavior is unclear.
- `/Users/randy/repos/forge/apps/web/src/` — UX reference for ambiguous
  decisions.
