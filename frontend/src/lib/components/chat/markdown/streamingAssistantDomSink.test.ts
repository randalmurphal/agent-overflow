import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import {
  AllowlistedPathCompletionGuard,
  createStreamingAssistantDomSink,
} from './streamingAssistantDomSink';
import type {
  StreamingAssistantParserCheckpoint,
  StreamingAssistantRevealSink,
} from '../../../stores/streamingAssistantReveal';
import StreamingAssistantDomSinkHarness from './StreamingAssistantDomSinkHarness.svelte';

function checkpointForSource(source: string): StreamingAssistantParserCheckpoint {
  let tailEnd = source.length;
  while (tailEnd > 0 && source.charCodeAt(tailEnd - 1) === 32) tailEnd--;
  const trailingAsciiSpaces = source.length - tailEnd;
  const tailStart = Math.max(0, tailEnd - 32);
  const tailSource = source.slice(tailStart, tailEnd);
  if (tailSource.length === 0) throw new Error('test checkpoint needs a literal tail');
  return {
    tailSource,
    tailStart: 0,
    tailEnd: tailSource.length,
    trailingAsciiSpaces,
  };
}

function canAppendLiteral(
  sink: StreamingAssistantRevealSink,
  source: string,
  nextSource: string,
  delta: string,
): boolean {
  return sink.canAppendLiteral(
    source,
    checkpointForSource(source),
    nextSource,
    delta,
  );
}

function restoreLiteral(
  sink: StreamingAssistantRevealSink,
  parserSource: string,
  source: string,
  directDeltas: readonly string[] = [source.slice(parserSource.length)],
): boolean {
  return sink.restoreLiteral(
    parserSource,
    checkpointForSource(parserSource),
    source,
    directDeltas,
  );
}

function makeRoot(text = 'seed'): {
  root: HTMLElement;
  host: HTMLSpanElement;
  base: Text;
} {
  const root = document.createElement('div');
  const volatile = document.createElement('div');
  volatile.className = 'md-volatile';
  const host = document.createElement('span');
  host.dataset.streamdownDirectAppendSafe = '';
  const base = document.createTextNode(text);
  host.append(base);
  volatile.append(host);
  root.append(volatile);
  document.body.append(root);
  return { root, host, base };
}

function expectGraphemeSafeNodeCuts(nodes: readonly Text[]): void {
  const text = nodes.map((node) => node.data).join('');
  const boundaries = new Set<number>([text.length]);
  for (const segment of new Intl.Segmenter('und', { granularity: 'grapheme' }).segment(text)) {
    boundaries.add(segment.index);
  }
  let offset = 0;
  for (const node of nodes.slice(0, -1)) {
    offset += node.length;
    expect(boundaries.has(offset), `Text-node cut ${offset} split a grapheme`).toBe(true);
  }
}

describe('streaming assistant DOM sink', () => {
  it('removes direct text without mutating the Svelte node on a consistent reset', () => {
    const { root, host, base } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    expect(host.textContent).toBe('seed next');
    expect(host.firstChild).toBe(base);
    expect(base.data).toBe('seed');
    expect(host.childNodes).toHaveLength(2);

    sink.reset();
    expect(host.textContent).toBe('seed');
    expect(host.childNodes).toHaveLength(1);
    expect(host.firstChild).toBe(base);
    root.remove();
  });

  it('does not desynchronize Svelte text when an authoritative leaf stays unchanged', async () => {
    const view = render(StreamingAssistantDomSinkHarness);
    const harness = view.component as unknown as {
      append(source: string, nextSource: string, delta: string): boolean;
      resetAndRender(text: string): Promise<void>;
    };

    expect(harness.append('seed ', 'seed next ', 'next ')).toBe(true);
    expect(view.container.textContent?.trim()).toBe('seed next');

    await harness.resetAndRender('seed');

    expect(view.container.textContent?.trim()).toBe('seed');
  });

  it('does not invoke grapheme segmentation while the active node has room', () => {
    const segment = vi.spyOn(Intl.Segmenter.prototype, 'segment');
    try {
      const { root, host } = makeRoot();
      const sink = createStreamingAssistantDomSink({
        getRoot: () => root,
        canAppendSource: () => true,
      });
      expect(canAppendLiteral(sink, 'seed ', 'seed ordinary ', 'ordinary ')).toBe(true);
      sink.appendLiteral('seed ordinary ', 'ordinary ');
      expect(canAppendLiteral(sink,
        'seed ordinary ',
        'seed ordinary streamed words ',
        'streamed words ',
      )).toBe(true);
      sink.appendLiteral('seed ordinary streamed words ', 'streamed words ');

      expect(segment).not.toHaveBeenCalled();
      expect(host.textContent).toBe('seed ordinary streamed words');
      sink.reset();
      root.remove();
    } finally {
      segment.mockRestore();
    }
  });

  it('clears owned nodes and state when direct-node removal fails', () => {
    const { root, host, base } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    const direct = host.childNodes.item(1) as Text;
    direct.remove = () => { throw new Error('remove failed'); };

    expect(() => sink.reset()).toThrow('remove failed');
    expect(host.childNodes).toHaveLength(1);
    expect(host.firstChild).toBe(base);

    // The authoritative render that follows a reset owns the base node. Once
    // it lands, this same sink can start a fresh literal run.
    base.data = 'seed next';
    expect(canAppendLiteral(sink,
      'seed next ',
      'seed next again ',
      'again ',
    )).toBe(true);
    root.remove();
  });

  it('rejects a host changed between direct appends', () => {
    const { root, base } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    base.data = 'rewritten ';
    expect(canAppendLiteral(sink,
      'seed next ',
      'seed next word ',
      'word ',
    )).toBe(false);
    sink.reset();
    root.remove();
  });

  it('rejects an incomplete-parser suffix inside the trailing text leaf', () => {
    const { root } = makeRoot('Term\n: detail:');
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(canAppendLiteral(sink,
      'Term\n: detail',
      'Term\n: details',
      's',
    )).toBe(false);
    root.remove();
  });

  it('adopts a matching non-ASCII parser tail without inspecting the canonical rope', () => {
    const source = 'Prefix café 東京 👩‍💻 ';
    const { root, host } = makeRoot(source.trimEnd());
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    const checkpoint = checkpointForSource(source);
    const originalSlice = String.prototype.slice;
    const originalCharCodeAt = String.prototype.charCodeAt;
    const slice = vi.spyOn(String.prototype, 'slice').mockImplementation(function (
      this: string,
      start?: number,
      end?: number,
    ): string {
      if (String(this) === source) {
        throw new Error('host adoption sliced the canonical source');
      }
      return Reflect.apply(originalSlice, this, [start, end]) as string;
    });
    const charCodeAt = vi.spyOn(String.prototype, 'charCodeAt').mockImplementation(function (
      this: string,
      index: number,
    ): number {
      if (String(this) === source) {
        throw new Error('host adoption inspected the canonical source');
      }
      return Reflect.apply(originalCharCodeAt, this, [index]) as number;
    });
    try {
      expect(sink.canAppendLiteral(
        source,
        checkpoint,
        `${source}continues `,
        'continues ',
      )).toBe(true);
      sink.appendLiteral(`${source}continues `, 'continues ');
      expect(host.textContent).toBe(`${source}continues`);
    } finally {
      charCodeAt.mockRestore();
      slice.mockRestore();
      sink.reset();
      root.remove();
    }
  });

  it('restores sink-owned text onto a remounted parser checkpoint', () => {
    const first = makeRoot();
    let root = first.root;
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');

    first.root.remove();
    const second = makeRoot('seed');
    root = second.root;
    expect(restoreLiteral(sink, 'seed ', 'seed next ')).toBe(true);
    expect(second.host.textContent).toBe('seed next');
    expect(canAppendLiteral(sink,
      'seed next ',
      'seed next again ',
      'again ',
    )).toBe(true);
    sink.appendLiteral('seed next again ', 'again ');
    expect(second.host.textContent).toBe('seed next again');
    sink.reset();
    second.root.remove();
  });

  it('restores pending spaces without displaying a trailing whitespace node', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(restoreLiteral(sink, 'seed ', 'seed next   ')).toBe(true);
    expect(host.textContent).toBe('seed next');
    expect(canAppendLiteral(sink,
      'seed next   ',
      'seed next   word ',
      'word ',
    )).toBe(true);
    sink.appendLiteral('seed next   word ', 'word ');
    expect(host.textContent).toBe('seed next   word');
    sink.reset();
    root.remove();
  });

  it('does not duplicate source whitespace already present in the parser host', () => {
    const { root, host } = makeRoot('seed ');
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(canAppendLiteral(sink, 'seed ', 'seed word ', 'word ')).toBe(true);
    sink.appendLiteral('seed word ', 'word ');

    expect(host.textContent).toBe('seed word');
    sink.reset();
    root.remove();
  });

  it('rejects a parser host with trailing whitespace absent from the source', () => {
    const { root } = makeRoot('seed  ');
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(canAppendLiteral(sink, 'seed ', 'seed word ', 'word ')).toBe(false);
    root.remove();
  });

  it('never splits a Unicode code point across bounded direct Text nodes', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    const delta = `${'a'.repeat(254)}𐐀b `;

    expect(canAppendLiteral(sink, 'seed ', `seed ${delta}`, delta)).toBe(true);
    sink.appendLiteral(`seed ${delta}`, delta);

    expect(host.textContent).toBe(`seed ${delta.slice(0, -1)}`);
    for (const node of Array.from(host.childNodes)) {
      if (!(node instanceof Text) || node.length === 0) continue;
      const first = node.data.charCodeAt(0);
      const last = node.data.charCodeAt(node.length - 1);
      expect(first >= 0xdc00 && first <= 0xdfff).toBe(false);
      expect(last >= 0xd800 && last <= 0xdbff).toBe(false);
    }
    sink.reset();
    root.remove();
  });

  it('lets an unbroken word exceed the node bound instead of splitting its shaping run', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    const first = 'a'.repeat(255);
    const second = '𐐀 b ';

    expect(canAppendLiteral(sink, 'seed ', `seed ${first}`, first)).toBe(true);
    sink.appendLiteral(`seed ${first}`, first);
    expect(canAppendLiteral(sink,
      `seed ${first}`,
      `seed ${first}${second}`,
      second,
    )).toBe(true);
    sink.appendLiteral(`seed ${first}${second}`, second);

    expect(host.textContent).toBe(`seed ${first}𐐀 b`);
    const directNodes = Array.from(host.childNodes).slice(1) as Text[];
    expectGraphemeSafeNodeCuts(directNodes);
    expect(directNodes[0].length).toBeGreaterThan(256);
    expect(directNodes[0].data.endsWith('𐐀')).toBe(true);
    sink.reset();
    root.remove();
  });

  it.each([
    ['combining mark', 'a', '\u0301'],
    ['emoji joiner', '👩', '\u200d💻'],
    ['regional-indicator pair', '🇺', '🇸'],
  ])('keeps a %s in one Text node when a later delta extends the full node', (
    _name,
    clusterStart,
    clusterEnd,
  ) => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    // The direct node begins with the checkpoint's pending source space.
    const fill = 'a'.repeat(255 - clusterStart.length) + clusterStart;
    expect(canAppendLiteral(sink, 'seed ', `seed ${fill}`, fill)).toBe(true);
    sink.appendLiteral(`seed ${fill}`, fill);
    expect((host.lastChild as Text).length).toBe(256);

    const continuation = `${clusterEnd} `;
    expect(canAppendLiteral(sink,
      `seed ${fill}`,
      `seed ${fill}${continuation}`,
      continuation,
    )).toBe(true);
    sink.appendLiteral(`seed ${fill}${continuation}`, continuation);

    const directNodes = Array.from(host.childNodes).slice(1) as Text[];
    expect(host.textContent).toBe(`seed ${fill}${clusterEnd}`);
    expectGraphemeSafeNodeCuts(directNodes);
    expect(directNodes[0].data.endsWith(clusterStart + clusterEnd)).toBe(true);
    sink.reset();
    root.remove();
  });

  it('allows one grapheme to exceed the nominal node bound rather than splitting it', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    const oversized = `e${'\u0301'.repeat(300)}`;
    const delta = `${oversized}x `;

    expect(canAppendLiteral(sink, 'seed ', `seed ${delta}`, delta)).toBe(true);
    sink.appendLiteral(`seed ${delta}`, delta);

    const directNodes = Array.from(host.childNodes).slice(1) as Text[];
    expectGraphemeSafeNodeCuts(directNodes);
    expect(directNodes.some((node) => node.length > 256)).toBe(true);
    expect(host.textContent).toBe(`seed ${oversized}x`);
    sink.reset();
    root.remove();
  });

  it('rejects a restore whose delta proof does not cover the canonical source', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(restoreLiteral(sink, 'seed ', 'rewritten ', [])).toBe(false);
    expect(host.textContent).toBe('seed');
    expect(host.childNodes).toHaveLength(1);
    root.remove();
  });
});

describe('AllowlistedPathCompletionGuard', () => {
  it('detects a path completed across a direct delta boundary', () => {
    const guard = new AllowlistedPathCompletionGuard();
    expect(guard.completes(
      [{ path: 'README' }],
      'Open READ',
      'Open README ',
      'ME ',
    )).toBe(true);
  });

  it('ignores paths already complete before the delta', () => {
    const guard = new AllowlistedPathCompletionGuard();
    expect(guard.completes(
      [{ path: 'README' }],
      'Open README and ',
      'Open README and continue ',
      'continue ',
    )).toBe(false);
  });

  it('tracks only the crossing tail across consecutive direct deltas', () => {
    const guard = new AllowlistedPathCompletionGuard();
    const refs = [{ path: 'README' }];

    expect(guard.completes(refs, 'Open R', 'Open REA', 'EA')).toBe(false);
    expect(guard.completes(refs, 'Open REA', 'Open README ', 'DME ')).toBe(true);
    expect(guard.completes(
      refs,
      'Open README ',
      'Open README and continue ',
      'and continue ',
    )).toBe(false);
  });

  it('rebuilds its crossing tail after an authoritative source rewrite', () => {
    const guard = new AllowlistedPathCompletionGuard();
    const refs = [{ path: 'README' }];

    guard.completes(refs, 'Old R', 'Old REA', 'EA');
    expect(guard.completes(refs, 'New READ', 'New README ', 'ME ')).toBe(true);
  });

  it('rebuilds its bound when the path-ref collection changes identity', () => {
    const guard = new AllowlistedPathCompletionGuard();
    guard.completes([{ path: 'short' }], 'Open sho', 'Open shor', 'r');

    expect(guard.completes(
      [{ path: 'a/much/longer/path' }],
      'Open a/much/longer/pa',
      'Open a/much/longer/path ',
      'th ',
    )).toBe(true);
  });

  it('clears its retained crossing tail while the allowlist is empty', () => {
    const guard = new AllowlistedPathCompletionGuard();
    const refs = [{ path: 'README' }];
    guard.completes(refs, 'Open READ', 'Open READM', 'M');
    expect(guard.completes([], 'unrelated', 'unrelated text ', ' text ')).toBe(false);

    expect(guard.completes(
      refs,
      'Fresh READ',
      'Fresh README ',
      'ME ',
    )).toBe(true);
  });
});
