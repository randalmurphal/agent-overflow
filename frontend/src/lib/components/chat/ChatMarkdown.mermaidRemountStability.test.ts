import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import {
  __resetRenderedHeightCachesForTest,
  readMermaidRenderedHeight,
  writeMermaidRenderedHeight,
} from './markdown/renderedHeightCache';
import {
  FAKE_INNER_HEIGHT,
  FAKE_OUTER_HEIGHT,
  FAKE_SOURCE_SWAP_HEIGHT,
  FAKE_STALE_RETRY_HEIGHT,
  installRenderedHeightGeometryStubs,
  insertRenderedMermaidSvg,
  overrideAnimationFrameWithManualFlush,
  overrideAnimationFrameWithTimeout,
  overrideOffsetHeight,
  resetRenderedHeightTestOverrides,
} from './renderedHeightTestHelpers';
import ChatMarkdownRemountHarness from './ChatMarkdownRemountHarness.svelte';
import StreamdownHostSourceSwapHarness from './StreamdownHostSourceSwapHarness.svelte';

// Mermaid counterpart to `ChatMarkdown.remountStability.test.ts`.
// The root invariant is the same: once a diagram has rendered, remounts
// of the same source should start with a cached rendered-height pin
// instead of revisiting the source-fallback/empty-SVG transient.

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

describe('<ChatMarkdown> mermaid host: cached min-height emitted on mount', () => {
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

  it('measure path writes the inner [data-streamdown-mermaid] offsetHeight once the SVG lands', async () => {
    const diagramSource = 'graph TD\n  C[measure-inner-mermaid] --> D[end]';
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
    insertRenderedMermaidSvg(wrapper);

    await waitFor(() => {
      expect(readMermaidRenderedHeight(diagramSource)).toBe(FAKE_INNER_HEIGHT);
    });

    r.unmount();
  });

  it('retries a zero-height SVG-ready read and uses the positive inner height on remount', async () => {
    overrideAnimationFrameWithTimeout();
    let innerHeightReads = 0;
    overrideOffsetHeight(function (this: HTMLElement) {
      if (this.hasAttribute?.('data-streamdown-mermaid')) {
        innerHeightReads += 1;
        return innerHeightReads === 1 ? 0 : FAKE_INNER_HEIGHT;
      }
      if (this.classList?.contains('mermaid-host-with-fallback')) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    });

    const diagramSource = 'graph TD\n  E[layout-settles] --> F[end]';
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
    insertRenderedMermaidSvg(wrapper);

    await waitFor(() => {
      expect(readMermaidRenderedHeight(diagramSource)).toBe(FAKE_INNER_HEIGHT);
    });
    expect(innerHeightReads).toBeGreaterThan(1);

    await r.rerender({
      source: '```mermaid\n' + diagramSource + '\n```',
      show: false,
    });
    await r.rerender({
      source: '```mermaid\n' + diagramSource + '\n```',
      show: true,
    });

    const remountedWrapper = r.container.querySelector(
      '.mermaid-host-with-fallback',
    ) as HTMLElement | null;
    expect(remountedWrapper).not.toBeNull();
    expect(
      remountedWrapper!.style.getPropertyValue('--mermaid-cached-min-h'),
    ).toBe(`${FAKE_INNER_HEIGHT}px`);

    r.unmount();
  });

  it('cancels a delayed zero-height measurement when the mermaid source changes before retry', async () => {
    const frame = overrideAnimationFrameWithManualFlush();
    let innerHeightReads = 0;
    overrideOffsetHeight(function (this: HTMLElement) {
      if (this.hasAttribute?.('data-streamdown-mermaid')) {
        innerHeightReads += 1;
        if (innerHeightReads === 1) return 0;
        if (innerHeightReads === 2) return FAKE_SOURCE_SWAP_HEIGHT;
        return FAKE_STALE_RETRY_HEIGHT;
      }
      if (this.classList?.contains('mermaid-host-with-fallback')) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    });

    const oldSource = 'graph TD\n  Old[delayed retry] --> End[end]';
    const newSource = 'graph TD\n  New[delayed retry] --> End[end]';
    const r = render(StreamdownHostSourceSwapHarness, {
      props: { kind: 'mermaid', source: oldSource },
    });

    await waitFor(() => {
      expect(r.container.querySelector('.mermaid-host-with-fallback')).not.toBeNull();
    });

    const wrapper = r.container.querySelector(
      '.mermaid-host-with-fallback',
    ) as HTMLElement;
    insertRenderedMermaidSvg(wrapper);
    await waitFor(() => {
      expect(innerHeightReads).toBe(1);
    });

    await r.rerender({ kind: 'mermaid', source: newSource });
    await waitFor(() => {
      expect(readMermaidRenderedHeight(newSource)).toBe(FAKE_SOURCE_SWAP_HEIGHT);
    });

    frame.flushAll();

    expect(readMermaidRenderedHeight(oldSource)).toBeUndefined();
    expect(readMermaidRenderedHeight(newSource)).toBe(FAKE_SOURCE_SWAP_HEIGHT);

    r.unmount();
  });
});
