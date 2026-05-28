import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import {
  __resetRenderedHeightCachesForTest,
  readMermaidRenderedHeight,
  writeMermaidRenderedHeight,
} from './markdown/renderedHeightCache';

// Regression coverage for the per-source rendered-height cache in
// `StreamdownMathHost` / `StreamdownMermaidHost`.
//
// The source-text fallback alone only protects the FIRST mount: it
// sizes the wrapper to the source length, KaTeX / mermaid inserts,
// the wrapper grows once. virtua remounts a row whenever it scrolls
// in or out of its rendered window, and each remount repeats the same
// "fallback height first, then rendered height" transient. To the
// scroll controller's contentRO it looks like a fresh negative-then-
// positive content delta (browser scroll-anchor auto-clamps on the
// dip, then the spring chases the regrow) — the symptom captured in
// `ui-trace/bookmarks/bug-report-20260528T162221Z.jsonl`, where
// `text:3:0`'s mermaid block resized `382→1418` seven times in 35s
// with descendant `mermaidHasSvg:true`.
//
// The fix: a module-level Map keyed by source caches the measured
// rendered height; on remount the host reads the cache via $derived
// and emits the value as a CSS variable (`--math-cached-min-h` /
// `--mermaid-cached-min-h`). A CSS rule applies that variable as
// `min-height` only while the wrapper is still in the
// `:not(.math-rendered)` / `:not(.mermaid-rendered)` state, so the
// pin holds during the brief gap between remount and renderer-content
// insertion and then drops — leaving the real layout to dictate
// post-render so viewport-width changes still take effect.
//
// happy-dom returns `0` for `offsetHeight` by default. The cache
// write skips zero-height measurements (so we never poison the cache
// with a layout-not-yet-settled value), so the math tests below stub
// a non-zero height on the host wrapper before render.
//
// Outer vs inner are distinct values on purpose: if a future change
// regresses `recordRenderedHeight` to measure the outer host (the
// grid-max of fallback + min-height-pin + inner — the bug shape from
// bug-report-20260528T172207Z), the cache would land at
// FAKE_OUTER_HEIGHT and the assertions expecting FAKE_INNER_HEIGHT
// would fail. The expected production cached value is the inner
// height alone.

const FAKE_OUTER_HEIGHT = 1500;
const FAKE_INNER_HEIGHT = 1418;
// Backwards-compatible alias for tests that don't care about the
// outer/inner distinction (i.e., they only mount and read variables
// derived from the populated cache).
const FAKE_RENDERED_HEIGHT = FAKE_INNER_HEIGHT;
let originalOffsetHeight: PropertyDescriptor | undefined;

beforeAll(() => {
  originalOffsetHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'offsetHeight',
  );
  Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
    configurable: true,
    get(this: HTMLElement) {
      // Inner renderer wrappers: the production
      // `recordRenderedHeight` measures these and that's what the
      // cache should store.
      if (
        this.hasAttribute?.('data-streamdown-block-math') ||
        this.hasAttribute?.('data-streamdown-mermaid')
      ) {
        return FAKE_INNER_HEIGHT;
      }
      // Outer host wrappers: deliberately a DIFFERENT value so a
      // measure-the-outer regression would write FAKE_OUTER_HEIGHT
      // to the cache and the assertions below would fail.
      if (
        this.classList?.contains('math-host-with-fallback') ||
        this.classList?.contains('mermaid-host-with-fallback')
      ) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    },
  });
});

afterAll(() => {
  if (originalOffsetHeight) {
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', originalOffsetHeight);
  } else {
    delete (HTMLElement.prototype as unknown as { offsetHeight?: unknown }).offsetHeight;
  }
});

// Module-level cache survives across tests in the same file; clear it
// in afterEach so a future test that reuses one of the source strings
// below can't accidentally read state from an earlier case.
afterEach(() => {
  __resetRenderedHeightCachesForTest();
});

const katexCalls: string[] = [];
vi.mock('katex', () => ({
  default: {
    renderToString: vi.fn((code: string) => {
      katexCalls.push(code);
      return `<span class="katex" data-rendered="${encodeURIComponent(code)}"></span>`;
    }),
  },
}));

import ChatMarkdownRemountHarness from './ChatMarkdownRemountHarness.svelte';

describe('<ChatMarkdown> block-math remount stability (rendered-height cache)', () => {
  it('first mount applies no cache variable; remount of the same source applies the cached min-height', async () => {
    katexCalls.length = 0;
    const source = '$$x_{remount-cache-math}^2$$';

    const r = render(ChatMarkdownRemountHarness, { props: { source, show: true } });

    await waitFor(() => {
      const wrapper = r.container.querySelector('.math-host-with-fallback');
      expect(wrapper?.classList.contains('math-rendered')).toBe(true);
    });

    const firstWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement;
    // First mount: $derived evaluated against an empty cache, so the
    // wrapper's inline style does NOT carry the variable. The
    // measure-after-render path then populates the cache (`offsetHeight`
    // stub above returns FAKE_RENDERED_HEIGHT for this wrapper).
    expect(firstWrapper.style.getPropertyValue('--math-cached-min-h')).toBe('');

    // Unmount.
    await r.rerender({ source, show: false });
    expect(r.container.querySelector('.math-host-with-fallback')).toBeNull();

    // Remount the same source. The module-level cache persists across
    // the {#if} unmount (same render root → same module instance), so
    // the fresh `StreamdownMathHost` instance's $derived sees the
    // cached value at construction and emits the CSS variable.
    await r.rerender({ source, show: true });

    const secondWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement | null;
    expect(secondWrapper).not.toBeNull();
    // Sync-init contract: with cache hit, the host's
    // `untrack(readMathRenderedHeight)` at init returns a defined
    // value, so `mathRendered` is `true` from frame 0 — BEFORE the
    // MutationObserver's first microtask. The class must therefore
    // be on synchronously after `rerender` returns; if sync-init
    // regresses to `$state(false)`, the class would only flip on
    // when the observer fires and this synchronous assertion would
    // fail.
    expect(secondWrapper!.classList.contains('math-rendered')).toBe(true);
    expect(secondWrapper!.style.getPropertyValue('--math-cached-min-h')).toBe(
      `${FAKE_RENDERED_HEIGHT}px`,
    );

    r.unmount();
  });

  it('different sources mount with their own cache lookup (no cross-contamination)', async () => {
    katexCalls.length = 0;
    const firstSource = '$$y_{cache-isolation-a}^2$$';
    const secondSource = '$$z_{cache-isolation-b}^3$$';

    const r = render(ChatMarkdownRemountHarness, {
      props: { source: firstSource, show: true },
    });
    await waitFor(() => {
      const wrapper = r.container.querySelector('.math-host-with-fallback');
      expect(wrapper?.classList.contains('math-rendered')).toBe(true);
    });

    // Swap source — Svelte tears down the old wrapper and mounts a
    // fresh one for the new source. The new source has no cache
    // entry, so its wrapper must NOT carry the variable even though
    // the first source's entry sits in the cache.
    await r.rerender({ source: secondSource, show: true });
    await waitFor(() => {
      const wrapper = r.container.querySelector('.math-host-with-fallback');
      expect(wrapper?.classList.contains('math-rendered')).toBe(true);
      expect(wrapper?.getAttribute('data-math-source')).toBe('z_{cache-isolation-b}^3');
    });
    const secondSourceWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement;
    expect(secondSourceWrapper.style.getPropertyValue('--math-cached-min-h')).toBe('');

    r.unmount();
  });
});

describe('<ChatMarkdown> block-math cache: zero-height measurements are skipped', () => {
  // This describe block does NOT inherit the offsetHeight stub from
  // the outer beforeAll (vitest's `beforeAll` is file-scoped, so the
  // stub IS in effect). To exercise the `h <= 0` early-return we
  // shadow the stub for this case with one that always returns 0.
  let shadowedOriginal: PropertyDescriptor | undefined;
  beforeAll(() => {
    shadowedOriginal = Object.getOwnPropertyDescriptor(
      HTMLElement.prototype,
      'offsetHeight',
    );
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
      configurable: true,
      get() {
        return 0;
      },
    });
  });
  afterAll(() => {
    if (shadowedOriginal) {
      Object.defineProperty(HTMLElement.prototype, 'offsetHeight', shadowedOriginal);
    }
  });

  it('does not poison the cache with a zero-height read', async () => {
    katexCalls.length = 0;
    const source = '$$omega_{zero-height-skip}^2$$';

    const r = render(ChatMarkdownRemountHarness, { props: { source, show: true } });
    await waitFor(() => {
      const wrapper = r.container.querySelector('.math-host-with-fallback');
      expect(wrapper?.classList.contains('math-rendered')).toBe(true);
    });

    // Unmount and remount. Cache should still be empty because the
    // first mount's measurement returned 0 and was skipped.
    await r.rerender({ source, show: false });
    await r.rerender({ source, show: true });
    await waitFor(() => {
      expect(r.container.querySelector('.math-host-with-fallback')).not.toBeNull();
    });

    const remountedWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement;
    // No cache entry → no CSS variable on the wrapper.
    expect(remountedWrapper.style.getPropertyValue('--math-cached-min-h')).toBe('');

    r.unmount();
  });
});

describe('<ChatMarkdown> mermaid host: cached min-height emitted on mount', () => {
  // Mermaid's renderer is fragile to mock under vitest's dynamic
  // imports (see `ChatMarkdown.cacheHits.test.ts` comment), so we
  // cover the host's cache-read path structurally: seed the cache for
  // a known mermaid source, mount ChatMarkdown with that source, and
  // assert the wrapper picks up the `--mermaid-cached-min-h` CSS
  // variable from the seeded value. This proves the host's
  // `$derived(readMermaidRenderedHeight(token.text))` is correctly
  // wired to the same cache the production write path populates.

  it("emits --mermaid-cached-min-h on the wrapper when the cache already has the source's height", async () => {
    const diagramSource = 'graph TD\n  A[seeded-height] --> B[end]';
    const seededHeight = 873;
    writeMermaidRenderedHeight(diagramSource, seededHeight);

    const r = render(ChatMarkdownRemountHarness, {
      props: { source: '```mermaid\n' + diagramSource + '\n```', show: true },
    });

    await waitFor(() => {
      expect(r.container.querySelector('.mermaid-host-with-fallback')).not.toBeNull();
    });

    const wrapper = r.container.querySelector(
      '.mermaid-host-with-fallback',
    ) as HTMLElement;
    expect(wrapper.getAttribute('data-mermaid-source')).toBe(diagramSource);
    expect(wrapper.style.getPropertyValue('--mermaid-cached-min-h')).toBe(
      `${seededHeight}px`,
    );

    r.unmount();
  });

  it('does not emit the variable when the cache has no entry for this source', async () => {
    const diagramSource = 'graph LR\n  P[unseeded-height] --> Q[end]';
    // No cache write — the cache is cleared by afterEach above.

    const r = render(ChatMarkdownRemountHarness, {
      props: { source: '```mermaid\n' + diagramSource + '\n```', show: true },
    });

    await waitFor(() => {
      expect(r.container.querySelector('.mermaid-host-with-fallback')).not.toBeNull();
    });

    const wrapper = r.container.querySelector(
      '.mermaid-host-with-fallback',
    ) as HTMLElement;
    expect(wrapper.style.getPropertyValue('--mermaid-cached-min-h')).toBe('');

    r.unmount();
  });

  it('measure path: writes the INNER [data-streamdown-mermaid] offsetHeight to the cache once the SVG lands (regression guard for outer-vs-inner asymmetry)', async () => {
    const diagramSource = 'graph TD\n  C[measure-inner-mermaid] --> D[end]';
    // Cache empty at start (afterEach reset).
    expect(readMermaidRenderedHeight(diagramSource)).toBeUndefined();

    const r = render(ChatMarkdownRemountHarness, {
      props: { source: '```mermaid\n' + diagramSource + '\n```', show: true },
    });

    await waitFor(() => {
      expect(r.container.querySelector('.mermaid-host-with-fallback')).not.toBeNull();
    });

    const wrapper = r.container.querySelector(
      '.mermaid-host-with-fallback',
    ) as HTMLElement;

    // svelte-streamdown's Mermaid renderer is hard to drive under
    // happy-dom (dynamic mermaid import, async SVG render), so we
    // simulate the post-render DOM shape directly: insert
    // `[data-streamdown-mermaid] > svg[data-mermaid-svg] > path`.
    // The host's MutationObserver watches `wrapperEl` for
    // `[data-mermaid-svg] *` additions; once we add the path
    // child the observer fires, `mermaidRendered` flips, and
    // `recordRenderedHeight` measures the inner wrapper (stub
    // returns FAKE_INNER_HEIGHT) and writes the cache.
    const inner =
      wrapper.querySelector<HTMLElement>('[data-streamdown-mermaid]') ??
      (() => {
        const el = document.createElement('div');
        el.setAttribute('data-streamdown-mermaid', '1');
        wrapper.appendChild(el);
        return el;
      })();
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('data-mermaid-svg', '1');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    svg.appendChild(path);
    inner.appendChild(svg);

    // Observer fires in the next microtask → recordRenderedHeight
    // writes FAKE_INNER_HEIGHT. If a future change flips
    // recordRenderedHeight back to measuring the OUTER wrapper,
    // it would write FAKE_OUTER_HEIGHT (1500) and this assertion
    // would fail.
    await waitFor(() => {
      expect(readMermaidRenderedHeight(diagramSource)).toBe(FAKE_INNER_HEIGHT);
    });

    r.unmount();
  });
});
