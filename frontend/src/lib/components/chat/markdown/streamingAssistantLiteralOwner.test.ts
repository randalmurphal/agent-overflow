import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import {
  attachStreamdownLiteralHost,
  type StreamdownLiteralHostHandle,
} from '../../../markdown';
import {
  AllowlistedPathCompletionGuard,
  createStreamingAssistantLiteralOwner,
} from './streamingAssistantLiteralOwner';
import type {
  StreamingAssistantParserCheckpoint,
  StreamingAssistantRevealSink,
} from '../../../stores/streamingAssistantReveal';
import StreamingAssistantLiteralOwnerHarness from './StreamingAssistantLiteralOwnerHarness.svelte';

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

/**
 * The production shape of an active literal host: the span renders EMPTY and
 * the literal-host controller is the single writer of its
 * children. `publish` is the authoritative parser update the renderer makes;
 * `base` is the node the controller created for it, which the owner adopts
 * rather than replaces.
 */
function makeRoot(text = 'seed'): {
  root: HTMLElement;
  host: HTMLSpanElement;
  base: Text;
  handle: StreamdownLiteralHostHandle;
  publish(next: string): void;
} {
  const root = document.createElement('div');
  const volatile = document.createElement('div');
  volatile.className = 'md-volatile';
  const host = document.createElement('span');
  host.dataset.streamdownDirectAppendSafe = '';
  volatile.append(host);
  root.append(volatile);
  document.body.append(root);
  const handle = attachStreamdownLiteralHost(host);
  handle.publish({}, text);
  return {
    root,
    host,
    base: host.firstChild as Text,
    handle,
    // A fresh token is what a re-lex produces: equal text under a new token is
    // still a structural change the controller has to reconcile.
    publish: (next: string) => handle.publish({}, next),
  };
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

describe('streaming assistant literal owner', () => {
  it('relinquishes the run on reset without removing any visible byte', () => {
    const { root, host, base } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    expect(host.textContent).toBe('seed next');
    expect(host.firstChild).toBe(base);
    expect(base.data).toBe('seed');
    expect(host.childNodes).toHaveLength(2);

    // The old sink deleted its siblings here, ahead of a Svelte flush that had
    // not happened yet — the visible string shrank back to the parser
    // checkpoint until the authoritative render caught up. Reset now
    // relinquishes the RUN only.
    sink.reset();
    expect(host.textContent).toBe('seed next');
    expect(host.childNodes).toHaveLength(2);
    expect(host.firstChild).toBe(base);
    root.remove();
  });

  it('extends in place when the authoritative update continues what it painted', () => {
    const { root, host, base, publish } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    const revealed = host.lastChild as Text;
    sink.reset();

    // The ordinary punctuation/structure fallback: the parser has caught up
    // with exactly what the reveal already painted, plus its own new bytes.
    publish('seed next word.');

    expect(host.textContent).toBe('seed next word.');
    // Nothing was torn down to get there: the nodes already on screen are the
    // SAME nodes, so no reader can have observed a shorter string.
    expect(host.firstChild).toBe(base);
    expect(base.data).toBe('seed');
    expect(Array.from(host.childNodes)).toContain(revealed);
    root.remove();
  });

  it('replaces a diverging authoritative update through the atomic primitive', () => {
    const { root, host, publish } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next longer ', 'next longer ')).toBe(true);
    sink.appendLiteral('seed next longer ', 'next longer ');
    sink.reset();

    // `replaceChildren` is the divergence primitive: the DOM `replace all`
    // algorithm queues ONE record carrying both the removals and the addition,
    // so no reader can see between the two halves. That RECORD-level guarantee
    // is a real-engine property — happy-dom decomposes the call into separate
    // records — and is asserted in
    // `ChatMarkdown.directRevealMonotonic.browser.test.ts`. What this test
    // pins is that the owner reaches for that primitive at all, instead of
    // removing its nodes and appending replacements.
    const replaceChildren = vi.spyOn(host, 'replaceChildren');
    let replaceCalls: unknown[][] = [];
    try {
      // Shorter than what is painted, so it cannot be reached by extending —
      // the genuine-divergence branch (a trimmed final summary).
      publish('seed trimmed');
      // Read the call log BEFORE restoring: `mockRestore` resets it.
      replaceCalls = replaceChildren.mock.calls.map((call) => [...call]);
    } finally {
      replaceChildren.mockRestore();
    }

    expect(host.textContent).toBe('seed trimmed');
    // One call, carrying the whole authoritative leaf as a single node.
    expect(replaceCalls).toHaveLength(1);
    expect(replaceCalls[0]).toHaveLength(1);
    expect((replaceCalls[0][0] as Text).data).toBe('seed trimmed');
    root.remove();
  });

  it('hands ownership back when the host detaches, in one task', async () => {
    const { root, host, handle } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    expect(handle.owned).toBe(true);

    // Settle: the literal host unmounts and releases its owner. Ownership
    // returns to the renderer with the reveal's bytes still on screen — the
    // settled static render is what replaces them, in its own flush.
    handle.detach();
    await Promise.resolve();

    expect(handle.owned).toBe(false);
    expect(host.textContent).toBe('seed next');
    // The released owner must not keep writing through a host it no longer
    // holds.
    expect(canAppendLiteral(sink, 'seed next ', 'seed next again ', 'again ')).toBe(false);
    root.remove();
  });

  it('takes ownership of the host when a run starts and keeps the renderer out', () => {
    const { root, host, handle, publish } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(handle.owned).toBe(false);

    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    expect(handle.owned).toBe(true);
    sink.appendLiteral('seed next ', 'next ');

    // While owned, an authoritative update is routed THROUGH the owner rather
    // than rendered by the controller, so the revealed suffix survives it.
    publish('seed next more');
    expect(host.textContent).toBe('seed next more');
    root.remove();
  });

  it('does not desynchronize Svelte text when an authoritative leaf stays unchanged', async () => {
    const view = render(StreamingAssistantLiteralOwnerHarness);
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
      const sink = createStreamingAssistantLiteralOwner({
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

  it('starts a fresh run against its own painted text after a fallback', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(canAppendLiteral(sink, 'seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');

    // A fallback relinquishes the run but leaves the bytes. The owner still
    // holds the host, so the next run adopts what IT painted — there is no
    // authoritative render in between to wait for. This is the whole
    // difference from the old sink, which needed Svelte to restate the text
    // before it could append again.
    sink.reset();
    expect(canAppendLiteral(sink,
      'seed next ',
      'seed next again ',
      'again ',
    )).toBe(true);
    sink.appendLiteral('seed next again ', 'again ');
    expect(host.textContent).toBe('seed next again');
    root.remove();
  });

  it('rejects a host changed between direct appends', () => {
    const { root, base } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(canAppendLiteral(sink, 'seed ', 'seed word ', 'word ')).toBe(false);
    root.remove();
  });

  it('never splits a Unicode code point across bounded direct Text nodes', () => {
    const { root, host } = makeRoot();
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
    const sink = createStreamingAssistantLiteralOwner({
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
