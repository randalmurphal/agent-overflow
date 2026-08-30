<script lang="ts">
  // Math host. Delegates KaTeX rendering to svelte-streamdown's
  // built-in Math component, but wraps the result in an element
  // tagged with the legacy `math-inline` / `math-display` class plus
  // `data-math-source` so the markdown-aware copy serializer
  // (`utils/markdownSerialize.ts`) can round-trip $...$ / $$...$$
  // back to source — KaTeX rewrites the inner DOM into typeset HTML,
  // so the LaTeX has to be stashed somewhere stable.
  //
  // Block math keeps a hidden source-text fallback inside the wrapper
  // until KaTeX has actually rendered into the inner DOM. svelte-
  // streamdown's `Math.svelte` imports KaTeX asynchronously on mount
  // and renders an empty inner wrapper until `katexInstance` lands,
  // so without the fallback the boundary commit moment goes
  // `deferred-host source pre → empty real wrapper → KaTeX html`.
  // That transient empty state caused contentEl to dip below the
  // streaming bottom, the browser auto-clamped scrollTop, and once
  // KaTeX landed the stick-to-bottom spring chased the full rendered
  // height from a stale lower scrollTop instead of just the
  // `rendered − source` delta — visible as a spring that started
  // from the top of the freshly-rendered block. Keeping the source
  // pre in the layout via CSS grid preserves height across the swap
  // so the spring only chases the actual size change.
  //
  // The `math-rendered` class we toggle below is also referenced by
  // a leftover `app.css` rule from the legacy enhanceMarkdown pipeline
  // (`.math-display.math-rendered { font-family: inherit }`). The
  // legacy rule is a no-op for us — the math wrapper inherits its
  // font already — so the overlap is harmless, but flagging it so a
  // future cleanup of the legacy CSS doesn't get a surprise.
  //
  import { untrack } from 'svelte';
  import Math from '../../../markdown/render/elements/Math.svelte';
  import { readMathRenderedHeight, writeMathRenderedHeight } from './renderedHeightCache';
  import { createRenderedHeightRecorder } from './renderedHeightMeasurement';

  // Per-source rendered-height cache. The source-text fallback alone
  // only protects the FIRST mount: it sizes the wrapper to the
  // source length, KaTeX inserts, the wrapper grows once. Windowing
  // remounts the row whenever it scrolls in/out of the rendered
  // window, and each remount repeats the same transient — fallback
  // height first, then KaTeX-rendered height — even though the
  // rendered height has already been observed at least once. That
  // repeated grow event looks identical to a fresh content delta on
  // the scroll controller's contentRO, triggering a spring chase on
  // every scroll-back-into-window. Caching the measured rendered
  // height per source lets us apply it as `min-height`
  // unconditionally on the wrapper (including AFTER `.math-rendered`
  // flips), so the remount lands pre-sized AND stays that way
  // through the fallback→KaTeX swap — paired with the sync
  // `mathRendered` init below, the wrapper never visits the
  // post-flip-but-pre-KaTeX collapse state that prior versions hit.
  // Trade-off: a viewport-width change between mounts that produces
  // a shorter render leaves the wrapper pinned slightly taller until
  // the next remount refreshes the cache — strictly better than the
  // alternative scroll-jump cycle (see bug-report-20260528T172207Z,
  // `text:4:0` 960 → 565 oscillation). For math this is largely
  // academic: KaTeX renders with width-independent height
  // (overflow-x-auto handles too-wide equations). Cache lives in a
  // sibling `.ts` module — see `renderedHeightCache.ts` for the
  // encapsulation and memory-bound details.

  // svelte-streamdown does not re-export `MathToken` at the package
  // root, only via internal `dist/marked` paths. Inline-shape it here
  // so we don't depend on that internal path; fields match the
  // library's MathToken (type: 'math', raw, text, isInline).
  type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
  };

  let { token, id }: { token: MathToken; id: string } = $props();
  const hostClass = $derived(token.isInline ? 'math-inline' : 'math-display');

  // Block-math swap tracking. `mathRendered` flips once KaTeX has put
  // a `.katex` node anywhere inside the wrapper — that's the signal
  // that the real renderer has produced its content and the source
  // fallback can be hidden.
  //
  // Initial value is derived from the cache: cache HIT → start true,
  // so the source fallback is `display: none` from frame 0 and the
  // wrapper sizes to the cached `min-height` until KaTeX content
  // replaces it. Cache MISS → start false so the source-text
  // fallback is the layout authority until the renderer lands. This
  // avoids the bug-report-20260528T172207Z regression where the
  // wrapper momentarily measured `max(fallback, min-height-pin)`
  // pre-flip then dropped to KaTeX-only post-flip: that swing
  // produced a row.resize on every remount (the `text:4:0`
  // 960 → 565 cycle in the trace).
  let wrapperEl: HTMLDivElement | undefined = $state();
  // Read the cached rendered height for this source. $derived so a
  // mid-stream token-text change re-evaluates the lookup; the cache
  // is a plain Map under the hood (non-reactive) so our own writes
  // during this instance don't re-fire this — by then KaTeX content
  // dictates layout and the cache is just for future remounts.
  const cachedRenderedHeight = $derived(readMathRenderedHeight(token.text));
  // `untrack` snapshots the initial cache read without binding to
  // later `token.text` changes — matches the codebase pattern for
  // "cache once at init" (cf. `GenericToolCallRow`, `AddProjectModal`,
  // `DirectoryBrowser`).
  let mathRendered = $state(
    untrack(() => readMathRenderedHeight(token.text) !== undefined),
  );

  $effect(() => {
    if (!wrapperEl) return;
    const sourceKey = token.text;
    // Measure the INNER math wrapper (the one KaTeX renders into),
    // not the outer host. The outer host's offsetHeight is the
    // grid max of `fallback`, `min-height-pin`, and the rendered
    // math wrapper — caching THAT value compounds across remounts.
    // The recorder waits until layout reports a positive inner height
    // so a renderer mutation that beats layout cannot leave the
    // remount cache empty.
    const heightRecorder = createRenderedHeightRecorder({
      root: () => wrapperEl,
      innerSelector: '[data-streamdown-block-math]',
      cacheKey: () => sourceKey,
      writeHeight: writeMathRenderedHeight,
    });
    const markRendered = () => {
      mathRendered = true;
      heightRecorder.record();
    };

    // Synchronous check first — when the KaTeX cache hits, the inner
    // html is already in the DOM by the time this effect runs and
    // we never need to attach an observer.
    if (wrapperEl.querySelector('.katex')) {
      markRendered();
      return heightRecorder.cancel;
    }
    const observer = new MutationObserver(() => {
      if (wrapperEl?.querySelector('.katex')) {
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

{#if token.isInline}
  <span class={hostClass} data-math-source={token.text}>
    <Math {token} {id} />
  </span>
{:else}
  <div
    bind:this={wrapperEl}
    class={[hostClass, 'math-host-with-fallback']}
    class:math-rendered={mathRendered}
    data-math-source={token.text}
    style:--math-cached-min-h={cachedRenderedHeight ? `${cachedRenderedHeight}px` : null}
  >
    <pre class="math-source-fallback" aria-hidden="true">{token.text}</pre>
    <Math {token} {id} />
  </div>
{/if}

<style>
  .math-host-with-fallback {
    display: grid;
  }
  .math-host-with-fallback > .math-source-fallback {
    grid-area: 1 / 1;
    margin: 0;
    /* Match the deferred host's `<pre>` so the swap from tail to
       prefix is visually identical. The browser default `<pre>` font
       and whitespace handling already line up; this just resets the
       margins so the wrapper height matches one-to-one. */
  }
  .math-host-with-fallback > :global([data-streamdown-block-math]) {
    grid-area: 1 / 1;
  }
  /* Once KaTeX has rendered, drop the source fallback out of the
     layout so the wrapper sizes to KaTeX (no stale whitespace below
     a short render). */
  .math-host-with-fallback.math-rendered > .math-source-fallback {
    display: none;
  }
  /* Until KaTeX renders, hide its empty wrapper so the fallback
     alone determines what the user sees. Once `math-rendered` flips
     the wrapper becomes visible and the fallback is removed. */
  .math-host-with-fallback:not(.math-rendered) > :global([data-streamdown-block-math]) {
    visibility: hidden;
  }
  /* Cached rendered-height pin. Defaults to `0` (no effect) on
     first-ever render so the natural fallback-then-grow path still
     runs exactly once. Applied UNCONDITIONALLY — including after
     `.math-rendered` flips — because the script sync-initializes
     `mathRendered = true` whenever the cache hits. Without this
     unconditional rule the math-rendered class drops the
     fallback (display:none) and the `:not(.math-rendered)`
     visibility:hidden on the inner wrapper, so the wrapper would
     collapse to inner=0 while KaTeX is loading async and then
     jump back up — the same oscillation the cache exists to
     prevent (see bug-report-20260528T172207Z). Trade-off
     documented in the script comment above. */
  .math-host-with-fallback {
    min-height: var(--math-cached-min-h, 0);
  }
</style>
