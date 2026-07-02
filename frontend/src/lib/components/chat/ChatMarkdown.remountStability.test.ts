import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import {
  __resetRenderedHeightCachesForTest,
  readMathRenderedHeight,
} from './markdown/renderedHeightCache';
import {
  FAKE_INNER_HEIGHT,
  FAKE_OUTER_HEIGHT,
  FAKE_RENDERED_HEIGHT,
  FAKE_SOURCE_SWAP_HEIGHT,
  FAKE_STALE_RETRY_HEIGHT,
  installRenderedHeightGeometryStubs,
  overrideAnimationFrameWithManualFlush,
  overrideAnimationFrameWithTimeout,
  overrideOffsetHeight,
  resetRenderedHeightTestOverrides,
} from './renderedHeightTestHelpers';

// Regression coverage for the per-source rendered-height cache in
// `StreamdownMathHost`.
//
// The source-text fallback alone only protects the FIRST mount: it
// sizes the wrapper to the source length, KaTeX inserts, the wrapper
// grows once. Windowing remounts a row whenever it scrolls in or out of
// the rendered window, and each remount repeats the same "fallback
// height first, then rendered height" transient. To the scroll
// controller's contentRO it looks like a fresh negative-then-positive
// content delta.
//
// The fix: a module-level Map keyed by source caches the measured
// rendered height; on remount the host reads the cache via $derived
// and emits the value as `--math-cached-min-h`. A CSS rule applies
// that variable as an unconditional `min-height`, and the host
// sync-initializes the rendered class on cache hit, so a remount
// never visits the source-fallback/empty-renderer transient.
//
// Outer vs inner are distinct values on purpose: if a future change
// regresses the rendered-height recorder to measure the outer host
// (the grid-max of fallback + min-height-pin + inner — the bug shape
// from bug-report-20260528T172207Z), the cache would land at
// FAKE_OUTER_HEIGHT and the assertions expecting FAKE_INNER_HEIGHT
// would fail. The expected production cached value is the inner
// height alone.

let restoreGeometryStubs: (() => void) | undefined;

beforeAll(() => {
  restoreGeometryStubs = installRenderedHeightGeometryStubs();
});

afterAll(() => {
  resetRenderedHeightTestOverrides();
  restoreGeometryStubs?.();
});

afterEach(() => {
  resetRenderedHeightTestOverrides();
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
import StreamdownHostSourceSwapHarness from './StreamdownHostSourceSwapHarness.svelte';

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
    expect(firstWrapper.style.getPropertyValue('--math-cached-min-h')).toBe('');

    await r.rerender({ source, show: false });
    expect(r.container.querySelector('.math-host-with-fallback')).toBeNull();

    await r.rerender({ source, show: true });

    const secondWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement | null;
    expect(secondWrapper).not.toBeNull();
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

  it('retries a zero-height first read and caches the first positive inner height for remount', async () => {
    katexCalls.length = 0;
    overrideAnimationFrameWithTimeout();
    let innerHeightReads = 0;
    overrideOffsetHeight(function (this: HTMLElement) {
      if (this.hasAttribute?.('data-streamdown-block-math')) {
        innerHeightReads += 1;
        return innerHeightReads === 1 ? 0 : FAKE_INNER_HEIGHT;
      }
      if (this.classList?.contains('math-host-with-fallback')) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    });

    const mathSource = 'alpha_{layout-settles}^2';
    const source = `$$${mathSource}$$`;

    const r = render(ChatMarkdownRemountHarness, { props: { source, show: true } });
    await waitFor(() => {
      expect(readMathRenderedHeight(mathSource)).toBe(FAKE_INNER_HEIGHT);
    });
    expect(innerHeightReads).toBeGreaterThan(1);

    await r.rerender({ source, show: false });
    await r.rerender({ source, show: true });

    const remountedWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement | null;
    expect(remountedWrapper).not.toBeNull();
    expect(remountedWrapper!.style.getPropertyValue('--math-cached-min-h')).toBe(
      `${FAKE_INNER_HEIGHT}px`,
    );

    r.unmount();
  });

  it('cancels a delayed zero-height measurement when the math source changes before retry', async () => {
    katexCalls.length = 0;
    const frame = overrideAnimationFrameWithManualFlush();
    let innerHeightReads = 0;
    overrideOffsetHeight(function (this: HTMLElement) {
      if (this.hasAttribute?.('data-streamdown-block-math')) {
        innerHeightReads += 1;
        if (innerHeightReads === 1) return 0;
        if (innerHeightReads === 2) return FAKE_SOURCE_SWAP_HEIGHT;
        return FAKE_STALE_RETRY_HEIGHT;
      }
      if (this.classList?.contains('math-host-with-fallback')) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    });

    const oldSource = 'beta_{old-delayed-retry}^2';
    const newSource = 'beta_{new-delayed-retry}^2';
    const r = render(StreamdownHostSourceSwapHarness, {
      props: { kind: 'math', source: oldSource },
    });

    await waitFor(() => {
      expect(innerHeightReads).toBe(1);
    });

    await r.rerender({ kind: 'math', source: newSource });
    await waitFor(() => {
      expect(readMathRenderedHeight(newSource)).toBe(FAKE_SOURCE_SWAP_HEIGHT);
    });

    frame.flushAll();

    expect(readMathRenderedHeight(oldSource)).toBeUndefined();
    expect(readMathRenderedHeight(newSource)).toBe(FAKE_SOURCE_SWAP_HEIGHT);

    r.unmount();
  });
});

describe('<ChatMarkdown> block-math cache: persistent zero-height measurements are not cached', () => {
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

    await r.rerender({ source, show: false });
    await r.rerender({ source, show: true });
    await waitFor(() => {
      expect(r.container.querySelector('.math-host-with-fallback')).not.toBeNull();
    });

    const remountedWrapper = r.container.querySelector(
      '.math-host-with-fallback',
    ) as HTMLElement;
    expect(remountedWrapper.style.getPropertyValue('--math-cached-min-h')).toBe('');

    r.unmount();
  });
});
