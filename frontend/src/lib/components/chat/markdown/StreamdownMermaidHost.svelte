<script lang="ts">
  // Mermaid host. Delegates rendering to svelte-streamdown's built-in
  // Mermaid component (which provides panzoom, fullscreen toggle, copy
  // and download). Stashes the original source on a wrapping element
  // tagged `mermaid` + `data-mermaid-source` so the markdown-aware
  // copy serializer (`utils/markdownSerialize.ts`) can round-trip the
  // diagram back to ` ```mermaid ` markdown — the rendered SVG itself
  // doesn't carry the source.
  //
  // Class names match the legacy enhanceMarkdown output so the
  // serializer's existing detection (`pre.dataset.mermaidSource` +
  // `:scope > svg` evidence) keeps working with one path tweak (the
  // host is a `<div>` rather than a `<pre>`).
  //
  // The `{#key}` wrapper on `<Mermaid>` forces a re-mount when the
  // app theme flips. svelte-streamdown reads its mermaid theme
  // (`'default'`/`'dark'`) from a MutationObserver on `html.light` /
  // `html.dark`, but the inner Mermaid component only calls
  // `mermaid.initialize(...)` at render time — already-rendered SVGs
  // stay in the prior palette until a remount. Theme toggles are
  // rare so the per-diagram re-render cost is acceptable.
  //
  // Same source-text fallback story as `StreamdownMathHost`: the
  // inner Mermaid component imports `mermaid` async on mount and
  // renders an empty placeholder until the import resolves AND the
  // SVG is rendered (`mermaid.render(...)` is itself async). Without
  // a fallback the wrapper collapses to 0 at the moment we swap the
  // deferred-host `<pre>` out for this real host, the browser
  // auto-clamps scrollTop on the dip, and the stick-to-bottom spring
  // chases the full SVG height from a stale lower scrollTop instead
  // of just the `svg − source` delta — visible as a spring starting
  // from the top of the freshly-rendered diagram. Holding the
  // source pre in the layout via CSS grid until the SVG has children
  // pins scrollTop through the swap so the spring only chases the
  // actual size change.

  import { untrack } from 'svelte';
  import Mermaid from 'svelte-streamdown/mermaid';
  import type { Tokens } from 'marked';
  import { getResolvedTheme } from '../../../stores/themeMode.svelte';
  import {
    readMermaidRenderedHeight,
    writeMermaidRenderedHeight,
  } from './renderedHeightCache';
  import { createRenderedHeightRecorder } from './renderedHeightMeasurement';

  // Per-source rendered-height cache. Same story as
  // StreamdownMathHost: the source-text fallback only covers the
  // first mount. Windowing remounts the row every time it scrolls in or
  // out of the rendered window; without a cached min-height the
  // wrapper collapses from the rendered SVG height back to the much
  // shorter fallback height on every remount, looking to the scroll
  // controller's contentRO like a fresh negative-then-positive
  // delta cycle (browser scroll-anchor auto-clamp on the dip, then
  // spring chase on the regrow). The cache lets each remount paint
  // pre-sized at the last observed rendered height; the pin is
  // applied UNCONDITIONALLY (including after `.mermaid-rendered`
  // flips), paired with sync-init `mermaidRendered = true` on cache
  // hit, so the wrapper never visits the post-flip-but-pre-SVG
  // collapse state.
  //
  // Trade-off — UNLIKE math, this is NOT academic for mermaid. The
  // math host gets away with the same unconditional pin because
  // KaTeX renders at width-independent heights (overflow-x-auto
  // handles too-wide equations). Mermaid SVGs are rendered with
  // `useMaxWidth: true`, so a width change DOES change the
  // rendered height. A user who resizes the viewport between two
  // remounts of the same diagram will see the wrapper pinned at
  // the prior width's height on the first post-resize remount,
  // producing a brief band of empty space below (if the new
  // render is shorter) or a single positive-delta growth (if
  // taller) before the cache refreshes from this mount's
  // measurement and matches on subsequent remounts. We accept
  // this because the alternative is the scroll-jump cycle this
  // cache exists to prevent (see math host's reference to
  // bug-report-20260528T172207Z): viewport resize is rare
  // compared to scroll-in/out cycles, and the artifact is
  // confined to a single remount per resize. Revisit by adding a
  // viewport-width bucket to the cache key if user feedback
  // shows the stale-pin is visible.
  //
  // Cache key is the diagram source alone. Theme is intentionally
  // excluded — a theme flip remounts the inner Mermaid via the
  // `{#key themeKey}` wrapper, and the wrapper sizes don't shift
  // visibly between themes (same diagram layout, different
  // palette). Cache lives in a sibling `.ts` module — see
  // `renderedHeightCache.ts`.

  let { token, id }: { token: Tokens.Code; id: string } = $props();
  const themeKey = $derived(getResolvedTheme());

  // SVG-presence tracking. Mermaid renders into `<svg data-mermaid-svg>`
  // via `svgTarget.innerHTML = svgString` — once the svg has children
  // we know the diagram is on screen and the source fallback can drop.
  let wrapperEl: HTMLDivElement | undefined = $state();

  // Read the cached rendered height for this source. $derived so a
  // mid-stream token-text change re-evaluates; the cache is a plain
  // Map under the hood (non-reactive), so writes during this
  // instance's lifetime do not refire this — by then mermaid content
  // dictates layout and the cache is just for future remounts.
  const cachedRenderedHeight = $derived(readMermaidRenderedHeight(token.text));

  // Initial value: cache HIT → start true so .mermaid-rendered is on
  // from frame 0 (paired with the unconditional CSS min-height pin);
  // cache MISS → start false so the source-text fallback owns the
  // layout until the observer flips us true. Avoids the
  // bug-report-20260528T172207Z regression shape applied to mermaid:
  // the wrapper momentarily measured `max(fallback, min-height-pin)`
  // pre-flip then dropped to SVG-only post-flip, producing a
  // row.resize on every remount. `untrack` snapshots the initial
  // cache read without binding to later `token.text` changes —
  // matches the codebase pattern for "cache once at init" (cf.
  // `GenericToolCallRow`, `AddProjectModal`, `DirectoryBrowser`).
  let mermaidRendered = $state(
    untrack(() => readMermaidRenderedHeight(token.text) !== undefined),
  );

  $effect(() => {
    if (!wrapperEl) return;
    const sourceKey = token.text;
    // Measure the INNER mermaid wrapper (the one svelte-streamdown
    // renders the SVG into), not the outer host. The outer host's
    // offsetHeight is the grid max of `fallback`, `min-height-pin`,
    // and the inner mermaid wrapper — caching THAT value compounds
    // across remounts. The recorder waits until layout reports a
    // positive inner height so the SVG mutation cannot race ahead of
    // layout and leave the remount cache empty. Same regression shape
    // as bug-report-20260528T172207Z applied to mermaid.
    const heightRecorder = createRenderedHeightRecorder({
      root: () => wrapperEl,
      innerSelector: '[data-streamdown-mermaid]',
      cacheKey: () => sourceKey,
      writeHeight: writeMermaidRenderedHeight,
    });
    const markRendered = () => {
      mermaidRendered = true;
      heightRecorder.record();
    };

    if (wrapperEl.querySelector('[data-mermaid-svg] *')) {
      markRendered();
      return heightRecorder.cancel;
    }
    const observer = new MutationObserver(() => {
      if (wrapperEl?.querySelector('[data-mermaid-svg] *')) {
        observer.disconnect();
        markRendered();
      }
    });
    observer.observe(wrapperEl, { childList: true, subtree: true });
    return () => {
      observer.disconnect();
      heightRecorder.cancel();
    };
  });
</script>

<div
  bind:this={wrapperEl}
  class={['mermaid', 'streamdown-mermaid-host', 'mermaid-host-with-fallback']}
  class:mermaid-rendered={mermaidRendered}
  data-mermaid-source={token.text}
  style:--mermaid-cached-min-h={cachedRenderedHeight ? `${cachedRenderedHeight}px` : null}
  onclickcapture={(e: MouseEvent) => {
    if (!(e.target instanceof Element)) return;
    if (!e.target.closest('[aria-label="Toggle expand"]')) return;
    e.stopPropagation();
    e.preventDefault();
    const wrapper = e.currentTarget as HTMLElement;
    const svg = wrapper.querySelector('svg[data-mermaid-svg]');
    if (svg) {
      document.dispatchEvent(
        new CustomEvent('diagram-expand', { detail: { html: svg.outerHTML } }),
      );
    }
  }}
>
  <pre class="mermaid-source-fallback" aria-hidden="true">{token.text}</pre>
  {#key themeKey}
    <Mermaid {token} {id} />
  {/key}
</div>

<style>
  .mermaid-host-with-fallback {
    display: grid;
  }
  .mermaid-host-with-fallback > .mermaid-source-fallback {
    grid-area: 1 / 1;
    margin: 0;
    /* Match the deferred host's `<pre>` so the swap from tail to
       prefix is visually identical. The browser default `<pre>` font
       and whitespace handling already line up; this just resets the
       margins so the wrapper height matches one-to-one. */
  }
  .mermaid-host-with-fallback > :global([data-streamdown-mermaid]) {
    grid-area: 1 / 1;
  }
  /* Once mermaid has rendered, drop the source fallback out of the
     layout so the wrapper sizes to the SVG (no stale whitespace
     below a short diagram). */
  .mermaid-host-with-fallback.mermaid-rendered > .mermaid-source-fallback {
    display: none;
  }
  /* Until the SVG lands, hide the empty inner wrapper so the
     fallback alone determines what the user sees. Once
     `mermaid-rendered` flips the wrapper becomes visible and the
     fallback is removed. */
  .mermaid-host-with-fallback:not(.mermaid-rendered) > :global([data-streamdown-mermaid]) {
    visibility: hidden;
  }
  /* Cached rendered-height pin. Defaults to `0` (no effect) on
     first-ever render so the natural fallback-then-grow path still
     runs exactly once. Applied UNCONDITIONALLY — including after
     `.mermaid-rendered` flips — because the script sync-initializes
     `mermaidRendered = true` whenever the cache hits. Without this
     unconditional rule the mermaid-rendered class drops the
     fallback (display:none) and the `:not(.mermaid-rendered)`
     visibility:hidden on the inner wrapper, so the wrapper would
     collapse to inner=0 while mermaid is loading async and then
     jump back up — the same oscillation the cache exists to
     prevent. Trade-off documented in the script comment above. */
  .mermaid-host-with-fallback {
    min-height: var(--mermaid-cached-min-h, 0);
  }
</style>
