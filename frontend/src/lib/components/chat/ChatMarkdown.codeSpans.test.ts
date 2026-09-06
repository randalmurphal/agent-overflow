import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import {
  __resetStreamdownCodeHostForTest,
  __streamdownCodeHostStatsForTest,
  appendCodeLines,
} from './markdown/StreamdownCodeHost.svelte';
import { createProvenAppend } from '../../markdown';
import {
  lineHashChain,
  resetLiveCodeSeedsForTest,
  type HighlightSeedEvent,
} from './markdown/liveCodeSeeds.svelte';
import { applyHighlightSeed } from '../../stores/eventsHighlight';
import { __resetAnimationFrameCoordinatorForTest } from '../../utils/animationFrameBatcher';

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
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword', 'string']);
});

it('appends code-line materialization from only the new suffix', () => {
  const lines = ['alpha', 'partial'];
  appendCodeLines(lines, ' tail\nbeta\n');
  expect(lines).toEqual(['alpha', 'partial tail', 'beta', '']);
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
    expect(container.querySelector('[data-code-source]')?.getAttribute('data-code-source')).toBe('');
  });

  it('unmounts the completed code component after its exact spans become cacheable', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    const markdown = await waitFor(() => {
      const found = container.querySelector('.md-committed');
      expect(found).not.toBeNull();
      expect(container.querySelector('.streamdown-code-host')).not.toBeNull();
      return found!;
    });
    const commentCount = (): number => {
      const walker = document.createTreeWalker(markdown, NodeFilter.SHOW_COMMENT);
      let count = 0;
      while (walker.nextNode()) count++;
      return count;
    };
    const mountedComments = commentCount();
    expect(mountedComments).toBeGreaterThan(2);

    await waitFor(() => expect(resolveHighlight).toBeTypeOf('function'));
    resolveHighlight(keywordSpans());
    await waitFor(() => expect(container.querySelector('.syntax-keyword')).not.toBeNull());
    await waitFor(() => expect(commentCount()).toBeLessThan(mountedComments / 2));

    expect(container.querySelector('[data-code-source] code')?.textContent).toBe(SOURCE);
  });

  it('does not force a live Selection read while retiring a cacheable code island', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    await waitFor(() => expect(container.querySelector('.streamdown-code-host')).not.toBeNull());

    const getSelection = vi.spyOn(document, 'getSelection');
    try {
      await waitFor(() => expect(resolveHighlight).toBeTypeOf('function'));
      resolveHighlight(keywordSpans());
      await waitFor(() => expect(container.querySelector('.syntax-keyword')).not.toBeNull());
      expect(getSelection).not.toHaveBeenCalled();
    } finally {
      getSelection.mockRestore();
    }
  });

  it('retires completed code without building the syntax-span DOM twice', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    const liveRoot = await waitFor(() => {
      const found = container.querySelector<HTMLElement>('.streamdown-code-host');
      expect(found).not.toBeNull();
      expect(found?.querySelector('.syntax-keyword')).toBeNull();
      return found!;
    });
    let reactiveSyntaxInsertion = false;
    const inspect = (records: MutationRecord[]): void => {
      for (const record of records) {
        if (!liveRoot.contains(record.target)) continue;
        for (const node of record.addedNodes) {
          if (
            node instanceof Element &&
            (node.matches('.syntax-keyword') || node.querySelector('.syntax-keyword'))
          ) reactiveSyntaxInsertion = true;
        }
      }
    };
    const observer = new MutationObserver(inspect);
    observer.observe(liveRoot, { subtree: true, childList: true });
    try {
      await waitFor(() => expect(resolveHighlight).toBeTypeOf('function'));
      resolveHighlight(keywordSpans());
      await waitFor(() => {
        expect(liveRoot.isConnected).toBe(false);
        expect(container.querySelector('.syntax-keyword')).not.toBeNull();
      });
      inspect(observer.takeRecords());
      expect(reactiveSyntaxInsertion).toBe(false);
    } finally {
      observer.disconnect();
    }
  });

  it('retires at most one completed code island per coordinated frame', async () => {
    __resetAnimationFrameCoordinatorForTest();
    let nextFrame = 0;
    const frames = new Map<number, FrameRequestCallback>();
    const requestFrame = vi.spyOn(globalThis, 'requestAnimationFrame').mockImplementation(
      (callback) => {
        const handle = ++nextFrame;
        frames.set(handle, callback);
        return handle;
      },
    );
    const cancelFrame = vi.spyOn(globalThis, 'cancelAnimationFrame').mockImplementation(
      (handle) => {
        frames.delete(handle);
      },
    );
    const highlightResolvers: Array<(value: ReturnType<typeof keywordSpans>) => void> = [];
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => highlightResolvers.push(resolve)),
    );
    const fence = '```python\n' + SOURCE + '\n```\n\n';
    const view = render(ChatMarkdown, {
      props: { source: fence.repeat(3), pathRefs: [] },
    });

    const runFrame = async (): Promise<void> => {
      const next = frames.entries().next().value as
        | [number, FrameRequestCallback]
        | undefined;
      if (!next) throw new Error('expected a coordinated animation frame');
      const [handle, callback] = next;
      frames.delete(handle);
      callback(performance.now());
      await tick();
      await Promise.resolve();
    };

    try {
      await waitFor(() => {
        expect(view.container.querySelectorAll('.streamdown-code-host')).toHaveLength(3);
        expect(highlightResolvers.length).toBeGreaterThan(0);
      });
      // Drain the initial cache-miss retry while highlighting remains pending.
      while (frames.size > 0) await runFrame();
      expect(view.container.querySelectorAll('[data-static-code-copy]')).toHaveLength(0);

      for (const resolve of highlightResolvers) resolve(keywordSpans());
      await waitFor(() => expect(frames.size).toBe(1));

      for (let retired = 1; retired <= 3; retired++) {
        await runFrame();
        expect(view.container.querySelectorAll('[data-static-code-copy]')).toHaveLength(retired);
        if (retired < 3) expect(frames.size).toBe(1);
      }
      expect(frames.size).toBe(0);
    } finally {
      view.unmount();
      __resetAnimationFrameCoordinatorForTest();
      requestFrame.mockRestore();
      cancelFrame.mockRestore();
    }
  });

  it('retires a ready code island while a sibling highlight remains pending', async () => {
    __resetAnimationFrameCoordinatorForTest();
    let nextFrame = 0;
    const frames = new Map<number, FrameRequestCallback>();
    const requestFrame = vi.spyOn(globalThis, 'requestAnimationFrame').mockImplementation(
      (callback) => {
        const handle = ++nextFrame;
        frames.set(handle, callback);
        return handle;
      },
    );
    const cancelFrame = vi.spyOn(globalThis, 'cancelAnimationFrame').mockImplementation(
      (handle) => {
        frames.delete(handle);
      },
    );
    const resolvers = new Map<string, (value: ReturnType<typeof keywordSpans>) => void>();
    setBindingMock(
      'HighlightCode',
      ({ source }: { source: string }) => new Promise((resolve) => {
        resolvers.set(source, resolve);
      }),
    );
    const ready = 'const ready = true;';
    const blocked = 'const blocked = true;';
    const view = render(ChatMarkdown, {
      props: {
        source:
          `\`\`\`python\n${ready}\n\`\`\`\n\n` +
          `\`\`\`python\n${blocked}\n\`\`\``,
        pathRefs: [],
      },
    });

    const runFrame = async (): Promise<void> => {
      const next = frames.entries().next().value as
        | [number, FrameRequestCallback]
        | undefined;
      if (!next) throw new Error('expected a coordinated animation frame');
      const [handle, callback] = next;
      frames.delete(handle);
      callback(performance.now());
      await tick();
      await Promise.resolve();
    };

    try {
      await waitFor(() => {
        expect(resolvers.has(ready)).toBe(true);
        expect(resolvers.has(blocked)).toBe(true);
      });
      while (frames.size > 0) await runFrame();

      resolvers.get(ready)!(keywordSpans());
      await waitFor(() => expect(frames.size).toBe(1));
      await runFrame();
      expect(view.container.querySelectorAll('[data-static-code-copy]')).toHaveLength(1);
      expect(view.container.querySelectorAll('[data-code-source] code')).toHaveLength(2);
      while (frames.size > 0) await runFrame();

      resolvers.get(blocked)!(keywordSpans());
      await waitFor(() => expect(frames.size).toBe(1));
      await runFrame();
      expect(view.container.querySelectorAll('[data-static-code-copy]')).toHaveLength(2);
    } finally {
      view.unmount();
      __resetAnimationFrameCoordinatorForTest();
      requestFrame.mockRestore();
      cancelFrame.mockRestore();
    }
  });

  it('postpones code-island retirement until focus leaves it', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    const markdown = await waitFor(() => {
      const found = container.querySelector('.md-committed');
      expect(found).not.toBeNull();
      return found!;
    });
    const commentCount = (): number => {
      const walker = document.createTreeWalker(markdown, NodeFilter.SHOW_COMMENT);
      let count = 0;
      while (walker.nextNode()) count++;
      return count;
    };
    const mountedComments = commentCount();
    const button = await waitFor(() => {
      const found = container.querySelector<HTMLButtonElement>('button[aria-label="Copy code"]');
      expect(found).not.toBeNull();
      return found!;
    });
    button.focus();
    expect(document.activeElement).toBe(button);

    await waitFor(() => expect(resolveHighlight).toBeTypeOf('function'));
    resolveHighlight(keywordSpans());
    await waitFor(() => expect(container.querySelector('.syntax-keyword')).not.toBeNull());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(commentCount()).toBeGreaterThan(mountedComments / 2);
    expect(button.isConnected).toBe(true);

    const outside = document.createElement('button');
    document.body.append(outside);
    try {
      outside.focus();
      await waitFor(() => expect(commentCount()).toBeLessThan(mountedComments / 2));
      expect(button.isConnected).toBe(false);
    } finally {
      outside.remove();
    }
  });

  it('postpones code-island retirement until a text selection clears', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });
    const markdown = await waitFor(() => {
      const found = container.querySelector('.md-committed');
      expect(found).not.toBeNull();
      return found!;
    });
    const commentCount = (): number => {
      const walker = document.createTreeWalker(markdown, NodeFilter.SHOW_COMMENT);
      let count = 0;
      while (walker.nextNode()) count++;
      return count;
    };
    const mountedComments = commentCount();
    const codeText = await waitFor(() => {
      const code = container.querySelector('code');
      if (!code) throw new Error('code element was not mounted');
      const walker = document.createTreeWalker(code, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (node.textContent?.includes('route')) return node as Text;
      }
      throw new Error('code source text was not mounted');
    });
    const selection = document.getSelection();
    if (!selection) throw new Error('test document has no Selection');
    selection.removeAllRanges();
    const range = document.createRange();
    range.setStart(codeText, 4);
    range.setEnd(codeText, 9);
    selection.addRange(range);
    document.dispatchEvent(new Event('selectionchange'));
    expect(selection.toString()).toBe('route');

    try {
      await waitFor(() => expect(resolveHighlight).toBeTypeOf('function'));
      resolveHighlight(keywordSpans());
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(commentCount()).toBeGreaterThan(mountedComments / 2);
      expect(selection.toString()).toBe('route');
      expect(container.querySelector('.syntax-keyword')).toBeNull();

      selection.removeAllRanges();
      document.dispatchEvent(new Event('selectionchange'));
      await waitFor(() => {
        expect(commentCount()).toBeLessThan(mountedComments / 2);
        expect(container.querySelector('.syntax-keyword')).not.toBeNull();
      });
    } finally {
      selection.removeAllRanges();
      document.dispatchEvent(new Event('selectionchange'));
    }
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

  it('copies settled code through one delegated button without retaining the source in an attribute', async () => {
    setBindingMock('HighlightCode', async () => keywordSpans());
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const { container } = render(ChatMarkdown, {
      props: { source: '```python\n' + SOURCE + '\n```', pathRefs: [] },
    });

    const button = await waitFor(() => {
      const found = container.querySelector<HTMLButtonElement>(
        'button[data-static-code-copy]',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    await fireEvent.click(button);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SOURCE));
    expect(button.getAttribute('aria-label')).toBe('Copied');
    expect(
      container.querySelector('[data-code-source]')?.getAttribute('data-code-source'),
    ).toBe('');
  });

  it('keeps an empty settled fence idle when its copy button is clicked', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const { container } = render(ChatMarkdown, {
      props: { source: '```\n```', pathRefs: [] },
    });

    const button = await waitFor(() => {
      const found = container.querySelector<HTMLButtonElement>(
        'button[data-static-code-copy]',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    await fireEvent.click(button);

    expect(writeText).not.toHaveBeenCalled();
    expect(button.getAttribute('aria-label')).toBe('Copy code');
  });

  it('reports a malformed delegated copy button instead of rejecting silently', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    try {
      const { container } = render(ChatMarkdown, {
        props: { source: '```\nsource\n```', pathRefs: [] },
      });
      const button = await waitFor(() => {
        const found = container.querySelector<HTMLButtonElement>(
          'button[data-static-code-copy]',
        );
        expect(found).not.toBeNull();
        return found!;
      });
      button.querySelector('[data-static-code-copy-icon]')?.remove();
      await fireEvent.click(button);

      await waitFor(() => {
        expect(error).toHaveBeenCalledWith(
          '[static-code-copy] handler failed',
          expect.any(Error),
        );
      });
      expect(writeText).toHaveBeenCalledWith('source');
    } finally {
      error.mockRestore();
    }
  });

  it('settled code keeps no per-line Svelte control nodes', async () => {
    setBindingMock('HighlightCode', async () => keywordSpans());
    const fences = Array.from(
      { length: 8 },
      (_, index) => `\`\`\`python\ndef block_${index}():\n    pass\n\`\`\``,
    ).join('\n\n');
    const { container } = render(ChatMarkdown, {
      props: { source: fences, pathRefs: [] },
    });

    const controlNodeCount = (host: Element): { comments: number; emptyText: number } => {
      const walker = document.createTreeWalker(
        host,
        NodeFilter.SHOW_COMMENT | NodeFilter.SHOW_TEXT,
      );
      let comments = 0;
      let emptyText = 0;
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (node.nodeType === Node.COMMENT_NODE) comments++;
        if (node.nodeType === Node.TEXT_NODE && node.textContent === '') emptyText++;
      }
      return { comments, emptyText };
    };
    await waitFor(() => {
      const hosts = container.querySelectorAll('.streamdown-code-host');
      expect(hosts).toHaveLength(8);
      for (const host of hosts) {
        expect(controlNodeCount(host)).toEqual({ comments: 0, emptyText: 0 });
      }
    });
  });

  it('does not split the full open-code source again for a proven append', async () => {
    const body = Array.from(
      { length: 300 },
      (_, index) => `const value_${index} = ${index};`,
    ).join('\n');
    const initial = `\`\`\`\n${body}`;
    const view = render(ChatMarkdown, {
      props: { source: initial, streaming: true, pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(body);
    });

    const delta = '\nconst appended = true;';
    const append = createProvenAppend(initial, delta);
    const nextCodeText = body + delta;
    const originalSplit = String.prototype.split;
    const splitImplementation = function (
      this: string,
      separator?: string | RegExp,
      limit?: number,
    ): string[] {
      if (separator === '\n' && String(this) === nextCodeText) {
        throw new Error('open code source was split from the beginning');
      }
      return Reflect.apply(originalSplit, this, [separator, limit]) as string[];
    };
    const split = vi
      .spyOn(String.prototype, 'split')
      .mockImplementation(splitImplementation as never);
    try {
      await view.rerender({
        source: append.next,
        sourceAppend: append,
        streaming: true,
        pathRefs: [],
      });
      await waitFor(() => {
        expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(
          nextCodeText,
        );
      });
    } finally {
      split.mockRestore();
    }
  });

  it('does not hash the full open-code source again for a proven append', async () => {
    const body = Array.from(
      { length: 300 },
      (_, index) => `const value_${index} = ${index};`,
    ).join('\n');
    const initial = `\`\`\`typescript\n${body}`;
    const view = render(ChatMarkdown, {
      props: { source: initial, streaming: true, pathRefs: [] },
    });
    await waitFor(() => {
      expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(body);
    });

    const delta = '\nconst appended = true;';
    const append = createProvenAppend(initial, delta);
    const nextCodeText = body + delta;
    const originalCharCodeAt = String.prototype.charCodeAt;
    const charCodeAt = vi.spyOn(String.prototype, 'charCodeAt').mockImplementation(function (
      this: string,
      index: number,
    ): number {
      if (String(this) === nextCodeText) {
        throw new Error('open code source was hashed from the beginning');
      }
      return Reflect.apply(originalCharCodeAt, this, [index]) as number;
    });
    try {
      await view.rerender({
        source: append.next,
        sourceAppend: append,
        streaming: true,
        pathRefs: [],
      });
      await waitFor(() => {
        expect(view.container.querySelector('[data-code-source] code')?.textContent).toBe(
          nextCodeText,
        );
      });
    } finally {
      charCodeAt.mockRestore();
    }
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
    // Non-append rerenders (for example, edited messages) can
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
