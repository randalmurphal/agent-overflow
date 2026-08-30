# Markdown renderer promotion (first-party streamdown + absorbed marked)

Status: approved 2026-08-29. Source analysis: the 2026-08-28 perf session
transcript review plus two investigation passes (repo survey, upstream
research). Decisions below are settled; do not re-litigate without the user.

## Why

`frontend/vendor/svelte-streamdown/` is a fork of a dead upstream
(beynar/svelte-streamdown HEAD == the 3.1.2 release, 2026-07-01, nothing
since). The divergence ledger reached 27 entries, ~1,900 lines of the tree
are already pure first-party code, and the vendoring ceremony (DIVERGENCE.md,
VENDOR.md, workspace package, `onwarn` suppression, patch files) costs
maintenance while buying nothing: there is no upstream to rebase against.
The `marked` dependency is lexer-only here (Parser/Renderer/`marked()` are
never reached) and is pinned to 16.4.2 by the vendor's `^16.2.1` range plus
a 266-line patch against a re-minified bundle that cannot survive a bump.

## Settled decisions

1. Promote the vendored tree to first-party source under
   `frontend/src/lib/markdown/` (parser) and a `render/` subtree. No dist
   artifacts, no divergence ledger. MIT notice survives per the
   `src/lib/markdown/boundary/LICENSE` precedent (incremark).
2. Absorb marked's lexer path (~1,900 lines: Lexer, Tokenizer, rules,
   helpers, token types) as first-party TS. Drop the `marked` package and
   `patches/marked@16.4.2.patch` entirely. Inline the patch's six
   allocation edits. Fix the blockquote-raw bug (divergence 15) at source
   and delete the boundary shim in `marked-alert`. Fold in marked 17/18
   fixes by hand: the two ReDoS fixes (17.0.4, 17.0.5) and the O(n^2)
   regex/masking fixes (18.0.4-18.0.11). Token-shape changes from v17/v18
   (checkbox child token, loose-list text->paragraph, trailing-newline
   space tokens) are adopted only if they simplify our renderer; our
   choice, not a migration.
3. Delete the vendor mermaid chrome (inline panzoom, MermaidDownload,
   save.js): our `DiagramModal` + `DiagramInteractionHost` (cursor-anchored
   zoom, pan, copy-PNG) is the surviving surface. Download-to-file can be
   added to the modal later if ever wanted.
4. Delete the footnote popover chrome (`FootnoteRef` popover, popover.svelte,
   useClickOutside, useKeyDown, floating-ui). `[^1]` refs keep rendering as
   chips.

   AMENDED after W2: there is no footnote list, so W1 left the definition
   body with no renderer at all — the chip pointed at nothing. The popup is
   back, REBUILT on the app's own primitives rather than restored:
   `chat/FootnotePopoverHost.svelte`, one instance for the whole app,
   opened by a delegated click on the chip and positioned by
   `primitives/Popover.svelte` (which portals out of the row, the thing
   floating-ui inside a containment-scoped row could not do). Nothing
   comes back into the vendor tree: the chip publishes
   `data-footnote-label`, `marked/index.js` gains one document-level
   `lexFootnoteDefinitions` query, and the rest is app code. Vendor
   DIVERGENCE entry 29. The same change gave `sup`/`sub` and the chip
   explicit, non-compounding sizes.
5. Keep the `citations` and `mdx` tokenizers even though their UI is
   overridden/disabled: removing them changes how `[foo]` and `<Foo/>`
   shaped prose parses. Rendering behavior stays identical.
6. Flatten the theme: one first-party table replaces vendor `theme.js` +
   `mergeTheme` + `streamdownTheme.ts` overrides. `shadcnTheme`, `clsx`,
   `tailwind-merge` all drop. This precedes the move (see W2 note).
7. Dependency bumps freed by the pin's death: katex 0.16.45 -> 0.18.4
   (21 CSS classes gained a `katex-` prefix, no compat layer; grep our CSS
   for the bare selectors first; 0.18.2 has a prototype-pollution fix) and
   mermaid 11.16.1 -> 11.17.2 exactly (`.edgePaths` regression in .0/.1;
   class diagrams flip to the v2 renderer by default; add
   `class: { defaultRenderer: 'dagre-d3' }` only if output regresses).
8. No visible rendering behavior changes anywhere in this campaign except
   those explicitly listed (mermaid chrome removal, footnote popover
   removal, and whatever the mermaid 11.17 class-renderer flip does if we
   accept it).

## Waves

Order is load-bearing. W1 -> W2 -> W3 -> W4 are sequential. W5 waits for
W3 (thread.svelte.ts imports from the moved barrel). W6 is independent and
may run in parallel with W1.

### W1 - dead-code deletion inside the vendor tree

Delete, with no behavior change (~3,100 lines):

- Group A (hard-unreachable): `Elements/index.{js,d.ts}`, `Elements/Code.svelte`
  (+ .d.ts), `utils/hightlighter.svelte.js` (+ .d.ts), `utils/bundledLanguages.*`.
  Highlighting flows through Go tree-sitter -> `StreamdownCodeHost`; zero
  shiki in the production bundle (verified against `dist/assets`).
- Group B (config-disabled/overridden): `Elements/TableDownload.svelte`,
  `Elements/Citation.svelte` (component only - NOT `marked-citations.js`),
  `Elements/stepperState.svelte.*`, `utils/get.*`, `utils/copy.svelte.*`.
- Group C (decision 3): `Elements/MermaidDownload.svelte`,
  `utils/panzoom.svelte.*`, `utils/save.*`, the fullscreen/panzoom wiring in
  `Elements/Mermaid.svelte` and the `controls.mermaid*` plumbing.
- Group D (decision 4): the popover in `Elements/FootnoteRef.svelte` (keep
  the superscript rendering), `Elements/popover.svelte.*`,
  `utils/useClickOutside.*`, `utils/useKeyDown.*`, `Elements/icons.*` if
  then unreferenced.
- Group E (never-taken branches): `Elements/fallbacks/*`,
  `AnimatedText.svelte` + the `animation` prop plumbing,
  `utils/darkMode.svelte.*` (app always passes `mermaidConfig.theme`).
- Group G: `shadcnTheme` inside `theme.js` (the rest of theme.js falls in W2).
- Also delete the stale `node_modules/.pnpm_patches/svelte-streamdown@3.1.2/`
  edit tree (pre-vendoring artifact; contains dropped fixes nobody wants).

None of this code is covered by tests, so the gate is a behavioral
verification pass over live rendering surfaces (code block, table, mermaid
diagram + expand modal, math, footnote, alert, citation-shaped prose,
image, link) plus the full existing suite staying green.

### W2 - theme flattening

One flat first-party theme table with the merged result of vendor `theme`
(tailwind base) + `streamdownTheme.ts` (overrides 31 of 32 groups).
`mergeTheme`/`cn` delete; `clsx` + `tailwind-merge` drop; divergence 20's
memoization becomes moot. Write the flat-table completeness test BEFORE
flattening (`streamdownTheme.test.ts` and `streamdownThemeMemo.test.ts`
lapse during the rewrite). Class strings must use app theme tokens only:
after W3 the code sits inside `src/` where `themeTokens.test.ts` (empty
allowlist) bans raw palette utilities - this is why W2 precedes W3.

### W3 - the move

Target layout:

```
src/lib/markdown/
  LICENSE            MIT: svelte-streamdown (Arnaud Derbey 2024) +
                     marked (MarkedJS 2018-2025, Christopher Jeffrey
                     2011-2018, landing in W4) + what is original
  index.ts           barrel: exactly the symbols the app imports today
  parser/
    lexer.ts             rule overrides as literals, extension registry
    provenAppend.ts
    parseBlocks.ts       split: core / cache / append-geometry
    parseBlocks.cache.ts
    parseBlocks.geometry.ts
    incrementalLex.ts
    incompleteMarkdown.ts  split by completer family if over ceiling
    extensions/          alert, blockquoteSource, br, citations, dl,
                         footnotes, hr, list, math, mdx, subsup, table,
                         tableSource, align
  render/
    Streamdown.svelte    extract context factory + settle bookkeeping
    Block.svelte
    CompactBlocks.svelte extract island lifecycle if over target
    LiteralHost.svelte + literalHost.ts
    staticHtml.ts  paragraphSpacing.ts  documentInteraction.ts
    context.svelte.ts
    theme.ts             the W2 flat table
    elements/            Element, Alert, Link, Image, Slot, Math, Mermaid,
                         FootnoteRef, url.ts
```

Mechanics:

- Collapse each `.js` + `.d.ts` pair into one `.ts` (35 pairs); delete the
  19 `.svelte.d.ts` sidecars (the `.svelte` files are already `lang="ts"`).
- Split files over the ceiling (conventions.md): `marked/index.js` 1591,
  `parse-incomplete-markdown.js` 1020, `Streamdown.svelte` 563; shrink
  `Element.svelte`/`Mermaid.svelte`/`CompactBlocks.svelte` toward the
  300 target where W1 already thinned them.
- Rewrite 38 import statements (36 bare `'svelte-streamdown'`, 2 subpaths
  in `StreamdownMathHost.svelte` / `StreamdownMermaidHost.svelte`).
- Config: drop the workspace dep + `pnpm-workspace.yaml` `packages:`;
  rewrite the `markdown-vendor` chunk regex in `vite.config.ts` (~line 86)
  to match the new path - verify the chunk still exists in `dist/` after
  build, otherwise 237KB falls into the entry bundle; delete the vendor
  `onwarn` block in `svelte.config.js` and fix every surfaced warning;
  delete `@source not "../vendor"` from `app.css`; keep the katex CSS alias
  in `vitest.config.ts`.
- Do NOT rename any `data-streamdown-*` / `md-blk` / `sd-*` attribute or
  class (26 files + `app.css:589-694` + `styleInvalidation.test.ts` key on
  them). Renames are out of scope.
- First type-check: this code has never been under `svelte-check`
  (vendor excluded from tsconfig). Expect a real round of strict-mode
  errors, concentrated in the parser.
- Retire DIVERGENCE.md/VENDOR.md. Divergences that document permanent
  behavior rules (2, 12, 15, 17, 21, 22, 24-27) become code comments at
  the site or entries in the new `src/lib/markdown/AGENTS.md`; the rest die
  with the ledger. Divergence 17 (path-relative hrefs never render raw
  anchors) is a security boundary cited by
  `docs/specs/remote-access-boundaries.md` - it must survive verbatim and
  stay documented.
- Sweep docs that reference the vendor path (frontend/AGENTS.md,
  chat/AGENTS.md, settle-flicker-analysis.md, theme-system.md,
  workflow-run-map.md, frontend-scroll.md, remote-access-boundaries.md).

### W4 - marked absorption + dep bumps

- Absorb Lexer/Tokenizer/rules/helpers/token types into
  `src/lib/markdown/parser/` (decision 2). Rule mutations from old
  divergence 2 become literals in the rules table. Delete the marked dep,
  the patch file, and the pnpm patchedDependencies entry.
- The four test files importing `{ Lexer }` from 'marked' switch to the
  first-party lexer; type-only imports switch to first-party token types.
- Then: katex -> 0.18.4 (grep CSS for the 21 renamed bare selectors first:
  accent base fix hdashline hline inner newline overlay overline root rule
  sizing smash sout stretchy strut tag thinbox underline vbox), mermaid ->
  11.17.2 (verify class-diagram output; `.edgePaths` CSS hooks unchanged).

### W5 - store/file splits (after W3)

Split with real seams, all callers updated, no shims:

- `frontend/src/lib/stores/thread.svelte.ts` (2,808)
- `frontend/src/lib/stores/threadStreamingReveal.svelte.ts` (1,267)
- `frontend/src/lib/stores/thread.svelte.test.ts` (10,829) - split by
  behavior area alongside the store split.

### W6 - invariants + test ergonomics (parallel-safe with W1)

- Write down and assert the reveal invariant: nothing may publish
  assistant row text ahead of an active smoother's cursor (five separate
  bugs in the perf session were this one rule; aad27067 is the latest
  instance). Dev-mode assertion at the row-replacement chokepoint + a
  regression test.
- Transition-test helper: drive an API on->off->on / called-twice and
  assert state cleanliness; adopt it in at least the six spots the session
  found by hand (sink adoption, live-updates toggle, view-only bootstrap,
  virtualizer mode cleanup, scrollbar drag across target replacement,
  lexer completion-mode cache).
- Record the ResizeObserver lesson in frontend/AGENTS.md testing section:
  a globally suppressed engine warning is a defect ledger entry, not a
  config setting.
- Markdown test fixtures: replace repeated filler ("ordinary streamed
  words") with distinguishable tokens, or add a first-divergence matcher
  (report index + 80 chars context) - failures currently dump >6KB bodies.
- Env-gate the timing-contract tests (`incrementalLex` relative-cost,
  `parseBlocksWorkload` bounded-input) behind `AO_PERF_CONTRACT=1`.
- Scoped typecheck: `pnpm run check:file <paths>` for the per-edit loop;
  full `pnpm run check` stays the wave gate.
- Fix `pnpm ... prettier` failing (script references prettier without the
  devDependency - add it or delete the caller).
- Root-level script forwarders (root package.json with check/test
  forwarding to frontend/) so repo-root invocations fail helpfully.
- Triage the three flaky tests: `ChatMarkdown.directRevealSelection.browser`,
  `thread.svelte.test.ts > "reconciles a streaming upsert that jumps ahead"`,
  `PerItemSmoother.test.ts`.

## Gates

Every wave leaves `make go-build`, `make go-test`,
`cd frontend && pnpm run check`, `pnpm run build`, and the full frontend
suite green. W1/W3 additionally require the behavioral verification pass
over the rendering surfaces listed in W1. W3 requires the bundle-chunk
check. Pixels and interactions stay identical except decisions 3/4/7.
