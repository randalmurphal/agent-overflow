import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import {
  __resetStreamdownCodeHostForTest,
  __streamdownCodeHostStatsForTest,
} from './markdown/StreamdownCodeHost.svelte';
import {
  lineHashChain,
  resetLiveCodeSeedsForTest,
  type HighlightSeedEvent,
} from './markdown/liveCodeSeeds.svelte';
import { applyHighlightSeed } from '../../stores/eventsHighlight';

// Integration coverage for the backend-span code-block host
// (StreamdownCodeHost + codeSpanCache) mounted through a real
// Streamdown instance. Proves the wire round trip: fenced block →
// HighlightCode request → `syntax-<name>` class spans in the DOM,
// plus the copy contract (code textContent === fence source) and the
// plain-render degrade paths.

const SOURCE = 'def route():\n    pass';

function keywordSpans() {
  // Line 1: "def" (3 bytes) as class 1, rest plain. Line 2: plain.
  return {
    lang: 'python',
    lines: [{ r: [3, 1] }, {}],
    truncated: false,
  };
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetLiveCodeSeedsForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword', 'string']);
});

// Simulates a backend `highlight:seed` push (remote clients) through
// the real ingest path, waiting out its class-name-table await.
async function pushSeed(text: string, lines: object[], overrides: Partial<HighlightSeedEvent> = {}) {
  applyHighlightSeed({
    threadId: 't1',
    itemId: 'i1',
    lang: 'python',
    lineHashes: lineHashChain(text),
    lines: lines as HighlightSeedEvent['lines'],
    final: false,
    ...overrides,
  });
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('<ChatMarkdown> code-block spans', () => {
  it('renders backend spans as syntax classes and keeps textContent equal to the source', async () => {
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });

    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    expect(rpc).toHaveBeenCalledWith({ lang: 'python', source: SOURCE });
    const keyword = container.querySelector('.syntax-keyword');
    expect(keyword?.textContent).toBe('def');

    // Copy contract: exact partition + real newline text nodes.
    const code = container.querySelector('[data-code-source] code');
    expect(code?.textContent).toBe(SOURCE);
    expect(
      container.querySelector('[data-code-source]')?.getAttribute('data-code-source'),
    ).toBe(SOURCE);
  });

  it('renders plain text immediately and skips the RPC for language-less fences', async () => {
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    const { container } = render(ChatMarkdown, {
      props: { source: '```\nplain text block\n```', pathRefs: [] },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-code-source] code')?.textContent).toBe(
        'plain text block',
      );
    });
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(rpc).not.toHaveBeenCalled();
  });

  it('degrades to plain text when the highlight RPC rejects', async () => {
    setBindingMock('HighlightCode', async () => {
      throw new Error('backend down');
    });
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
    });
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(container.querySelector('.syntax-keyword')).toBeNull();
    expect(container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
  });

  it('reuses the cache for a remounted identical block (no second RPC)', async () => {
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    const first = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(first.container.querySelector('.syntax-keyword')).not.toBeNull();
    });
    first.unmount();

    // The settle-remount path: an identical block mounts fresh and
    // must paint highlighted from the synchronous cache hit.
    const second = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(second.container.querySelector('.syntax-keyword')).not.toBeNull();
    });
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('seeds a fresh mount from the previous instance while the exact result is pending', async () => {
    // The committed-prefix migration remounts a block without waiting
    // on span requests; the fresh instance must keep the previous
    // instance's stale-prefix colors instead of flashing plain.
    setBindingMock('HighlightCode', async () => keywordSpans());
    const first = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(first.container.querySelector('.syntax-keyword')).not.toBeNull();
    });
    first.unmount();

    // Cold span cache (evicted), extended source, and a gated RPC: the
    // only color source is the previous instance's adoption.
    resetCodeSpanCacheForTest();
    let release: ((value: ReturnType<typeof keywordSpans>) => void) | undefined;
    setBindingMock(
      'HighlightCode',
      () => new Promise<ReturnType<typeof keywordSpans>>((resolve) => { release = resolve; }),
    );
    const second = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\nx = 1\n```', pathRefs: [] },
    });
    // Synchronously seeded: the prefix line paints on first render,
    // before the (gated) exact request has resolved.
    expect(second.container.querySelector('.syntax-keyword')).not.toBeNull();

    await waitFor(() => expect(release).toBeDefined());
    release!({ lang: 'python', lines: [{ r: [3, 1] }, {}, {}], truncated: false });
    await waitFor(() => {
      expect(second.container.querySelector('[data-code-source] code')?.textContent).toBe(
        SOURCE + '\nx = 1',
      );
    });
  });

  it('adopts the result when a block is replaced with SHORTER source', async () => {
    // Non-append rerenders (design previews, edited messages) can
    // shrink a block; supersession is by request sequence, not source
    // length, so the shorter source's spans must land.
    const longSource = 'def much_longer_name():\n    pass';
    const shortSource = '"abc"';
    setBindingMock('HighlightCode', async (req: { source: string }) =>
      req.source === longSource
        ? keywordSpans()
        : { lang: 'python', lines: [{ r: [5, 2] }], truncated: false },
    );
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + longSource + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    await view.rerender({ source: '```python\n' + shortSource + '\n```', pathRefs: [] });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-string')?.textContent).toBe(shortSource);
    });
    expect(view.container.querySelector('.syntax-keyword')).toBeNull();
  });

  it('re-requests when the fence language changes for identical text', async () => {
    const rpc = setBindingMock('HighlightCode', async (req: { lang: string }) =>
      req.lang === 'python' ? keywordSpans() : { lang: 'go', lines: [{ r: [3, 2] }, {}], truncated: false },
    );
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    // Same text, new language: the old classes must not survive the
    // language change.
    await view.rerender({ source: '```go\n' + SOURCE + '\n```', pathRefs: [] });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-string')).not.toBeNull();
    });
    expect(rpc).toHaveBeenCalledWith({ lang: 'go', source: SOURCE });
    expect(view.container.querySelector('.syntax-keyword')).toBeNull();
  });

  it('never adopts superseded content over a newer synchronous adoption', async () => {
    // Sequence: cached content A → uncached content B (fired, in
    // flight) → back to A (sync cache hit). B's result must never
    // adopt afterwards — an undemoted in-flight fire would paint B's
    // spans against A's text.
    const staleContent = 'stale_content';
    let releaseStale: ((v: ReturnType<typeof keywordSpans>) => void) | undefined;
    setBindingMock('HighlightCode', async (req: { source: string }) => {
      if (req.source === staleContent) {
        return new Promise<ReturnType<typeof keywordSpans>>((resolve) => {
          releaseStale = resolve;
        });
      }
      return keywordSpans();
    });
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    await view.rerender({ source: '```python\n' + staleContent + '\n```', pathRefs: [] });
    await view.rerender({ source: '```python\n' + SOURCE + '\n```', pathRefs: [] });

    // Let any queued fire run, then resolve B with distinct classes.
    await new Promise((resolve) => setTimeout(resolve, 200));
    releaseStale?.({ lang: 'python', lines: [{ r: [staleContent.length, 2] }], truncated: false });
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(view.container.querySelector('.syntax-string')).toBeNull();
    expect(view.container.querySelector('.syntax-keyword')?.textContent).toBe('def');
    expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
  });

  it('renders a replacement block plain instead of applying stale spans when its request rejects', async () => {
    setBindingMock('HighlightCode', async () => keywordSpans());
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    // Unrelated replacement text (not an extension of the old source)
    // whose request fails: old spans must not be applied by index.
    setBindingMock('HighlightCode', async () => { throw new Error('down'); });
    const replacement = 'totally different\ncontent here';
    await view.rerender({ source: '```python\n' + replacement + '\n```', pathRefs: [] });
    await waitFor(() => {
      expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(
        replacement,
      );
    });
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(view.container.querySelector('.syntax-keyword')).toBeNull();
  });

  it('drains content that arrives while a demoted request is in flight', async () => {
    // Sequence: cached A → uncached B (fired, stalls in flight) →
    // back to A (sync adoption demotes B and clears the pending
    // debt) → uncached C while B is STILL in flight (schedule defers
    // to the in-flight request). When B finally settles, its drain
    // must fire C's request even though B's seq is stale — otherwise
    // C (and the held settle gate) strands until the next token
    // change, which never comes for final content.
    const settled = vi.fn();
    const bContent = 'stalled_b';
    const cContent = 'final_c';
    let releaseB: ((v: ReturnType<typeof keywordSpans>) => void) | undefined;
    setBindingMock('HighlightCode', async (req: { source: string }) => {
      if (req.source === bContent) {
        return new Promise<ReturnType<typeof keywordSpans>>((resolve) => {
          releaseB = resolve;
        });
      }
      if (req.source === cContent) {
        return { lang: 'python', lines: [{ r: [cContent.length, 2] }], truncated: false };
      }
      return keywordSpans();
    });
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
      context: new Map([[CHAT_MARKDOWN_SETTLED_CONTEXT, settled]]),
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });

    await view.rerender({ source: '```python\n' + bContent + '\n```', pathRefs: [] });
    await waitFor(() => expect(releaseB).toBeDefined());
    await view.rerender({ source: '```python\n' + SOURCE + '\n```', pathRefs: [] });
    settled.mockClear();
    await view.rerender({ source: '```python\n' + cContent + '\n```', pathRefs: [] });

    releaseB!({ lang: 'python', lines: [{ r: [bContent.length, 1] }], truncated: false });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-string')?.textContent).toBe(cContent);
    });
    await waitFor(() => expect(settled).toHaveBeenCalled());
  });

  it('releases the settle gate when current content needs no async work, despite a stalled stale request', async () => {
    // The registerAsyncResource gate must track the CURRENT content
    // only. Sequence: content A settles → content B's request stalls
    // forever → back to A (already exact, no async work). The gate
    // must release — Streamdown's onsettled drives the chat warm gate,
    // and a superseded request that never resolves must not block it.
    const settled = vi.fn();
    const staleContent = 'stale_content';
    setBindingMock('HighlightCode', async (req: { source: string }) =>
      req.source === staleContent ? new Promise<never>(() => {}) : keywordSpans(),
    );
    const view = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
      context: new Map([[CHAT_MARKDOWN_SETTLED_CONTEXT, settled]]),
    });
    await waitFor(() => {
      expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
    });
    await waitFor(() => expect(settled).toHaveBeenCalled());

    await view.rerender({ source: '```python\n' + staleContent + '\n```', pathRefs: [] });
    // Let the throttled fire dispatch so the stale request is in flight.
    await new Promise((resolve) => setTimeout(resolve, 150));
    settled.mockClear();

    await view.rerender({ source: '```python\n' + SOURCE + '\n```', pathRefs: [] });
    await waitFor(() => expect(settled).toHaveBeenCalled());
  });

  it('adopts an exact live seed without any RPC', async () => {
    // Remote-client fast path: a pushed seed whose hash chain covers
    // the whole block settles it — the round trip is skipped entirely.
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    await pushSeed(SOURCE, [{ r: [3, 1] }, {}]);

    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')?.textContent).toBe('def');
    });
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(rpc).not.toHaveBeenCalled();
  });

  it('paints a verified seed prefix while the exact request runs', async () => {
    // A seed for the first line only: the verified prefix must paint
    // immediately (including its LAST line — hash-verified complete,
    // unlike own stale results) while the exact request still fires.
    let release: ((v: ReturnType<typeof keywordSpans>) => void) | undefined;
    const rpc = setBindingMock(
      'HighlightCode',
      () => new Promise<ReturnType<typeof keywordSpans>>((resolve) => { release = resolve; }),
    );
    await pushSeed('def route():', [{ r: [3, 1] }]);

    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')?.textContent).toBe('def');
    });
    await waitFor(() => expect(rpc).toHaveBeenCalledWith({ lang: 'python', source: SOURCE }));

    release!(keywordSpans());
    await waitFor(() => {
      expect(container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
      expect(container.querySelector('.syntax-keyword')).not.toBeNull();
    });
  });

  it('re-matches when a seed arrives after mount', async () => {
    // The final seed can land AFTER the last token change (settle
    // without further deltas). The generation signal must re-run the
    // match so the block colors without waiting on the stalled RPC.
    setBindingMock('HighlightCode', () => new Promise<never>(() => {}));
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
    });
    expect(container.querySelector('.syntax-keyword')).toBeNull();

    await pushSeed(SOURCE, [{ r: [3, 1] }, {}]);
    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')?.textContent).toBe('def');
    });
  });

  it('uses the first info-string word as highlight identity for attributed fences', async () => {
    // marked's token.lang is the FULL info string ("python title=x");
    // spans and seeds key by its first word so the backend recognizes
    // the language and pushed seeds match. The stamp keeps the full
    // string for fence-faithful copy serialization.
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    const { container } = render(ChatMarkdown, {
      props: { source: '```python title=demo\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')).not.toBeNull();
    });
    expect(rpc).toHaveBeenCalledWith({ lang: 'python', source: SOURCE });
    expect(container.querySelector('[data-code-lang]')?.getAttribute('data-code-lang')).toBe(
      'python title=demo',
    );
  });

  it('matches a pushed seed against an attributed fence', async () => {
    // The backend fence scanner seeds under the first info-string word;
    // the host must look it up under the same identity.
    const rpc = setBindingMock('HighlightCode', async () => keywordSpans());
    await pushSeed(SOURCE, [{ r: [3, 1] }, {}]);

    const { container } = render(ChatMarkdown, {
      props: { source: '```python title=demo\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => {
      expect(container.querySelector('.syntax-keyword')?.textContent).toBe('def');
    });
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(rpc).not.toHaveBeenCalled();
  });

  it('does not memoize empty span sets for remount seeding', async () => {
    // Truncated over-cap results come back with no lines; remembering
    // them would retain the full dead source for nothing.
    setBindingMock('HighlightCode', async () => ({ lang: 'python', lines: [], truncated: true }));
    render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(__streamdownCodeHostStatsForTest().lastAdopted).toBe(0);
    expect(__streamdownCodeHostStatsForTest().chars).toBe(0);
  });

  it('bounds the remount-adoption memo', async () => {
    // Fence "languages" are arbitrary info-string text; the memo is an
    // 8-entry LRU so unique labels cannot retain sources forever.
    setBindingMock('HighlightCode', async () => ({ lang: 'x', lines: [{}], truncated: false }));
    const fences = Array.from(
      { length: 10 },
      (_, i) => '```lang' + i + '\nblock ' + i + '\n```',
    ).join('\n\n');
    render(ChatMarkdown, { props: { source: fences, pathRefs: [] } });
    await waitFor(() => {
      expect(__streamdownCodeHostStatsForTest().lastAdopted).toBeGreaterThan(0);
    });
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(__streamdownCodeHostStatsForTest().lastAdopted).toBeLessThanOrEqual(8);
  });
});
