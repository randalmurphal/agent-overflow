import { describe, expect, it, vi } from 'vitest';
import {
  buildRecallEntries,
  createComposerHistoryRecall,
  HISTORY_RECALL_LIMIT,
  recallArrowIntent,
  type ComposerHistoryRecallDeps,
  type RecallHistoryRow,
} from './composerHistoryRecall';
import { makeItem } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';

function row(id: string, turnIndex: number, summary: string, itemIndex = 0): RecallHistoryRow {
  return { id, turnIndex, itemIndex, summary };
}

function userItem(id: string, turnIndex: number, summary: string, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, turnIndex, itemIndex: 0, kind: 'user_text', role: 'user', summary, ...overrides });
}

describe('buildRecallEntries', () => {
  it('orders newest first: pending sends, then items by position', () => {
    const entries = buildRecallEntries(
      [row('u2', 1, 'second'), row('u1', 0, 'first')],
      [userItem('u3', 2, 'third')],
      ['queued oldest', 'queued newest'],
      '',
    );
    expect(entries).toEqual(['queued newest', 'queued oldest', 'third', 'second', 'first']);
  });

  it('overlays the loaded window over the backend baseline by position', () => {
    // The optimistic just-sent row shares a position with nothing in the
    // backend answer yet; an edited row at a known position wins over it.
    const entries = buildRecallEntries(
      [row('u1', 0, 'stale backend text')],
      [userItem('user:0', 0, 'fresh window text'), userItem('user:1', 1, 'optimistic send')],
      [],
      '',
    );
    expect(entries).toEqual(['optimistic send', 'fresh window text']);
  });

  it('applies the reader-authored predicate to window rows', () => {
    const entries = buildRecallEntries(
      [],
      [
        userItem('real', 1, 'real ask'),
        userItem('wire', 2, 'injected', { meta: '{"wire_only":true}' }),
        userItem('child', 3, 'child prompt', { parentId: 'real' }),
        makeItem({ id: 'a1', turnIndex: 4, kind: 'assistant_text', role: 'assistant', summary: 'reply' }),
      ],
      [],
      '',
    );
    expect(entries).toEqual(['real ask']);
  });

  it('strips attachment images, drops blanks, collapses consecutive duplicates', () => {
    const entries = buildRecallEntries(
      [
        row('u4', 3, 'look\n\n![img](attachment://abc)'),
        row('u3', 2, 'repeat'),
        row('u2', 1, 'repeat'),
        row('u1', 0, '   '),
      ],
      [],
      [],
      '',
    );
    expect(entries).toEqual(['look', 'repeat']);
  });

  it('never leads with an entry identical to the stash', () => {
    // The second row differs, so nothing collapses — only the stash
    // check can drop the lead. Deleting the shift fails this.
    const entries = buildRecallEntries(
      [row('u2', 1, 'typed already'), row('u1', 0, 'older ask')],
      [],
      [],
      'typed already',
    );
    expect(entries).toEqual(['older ask']);
  });

  it('strips inline image placeholder labels alongside the image blocks', () => {
    const entries = buildRecallEntries(
      [
        row('u2', 1, 'look at [Image #1] please\n\n![img](attachment://abc)'),
        row('u1', 0, '[Image #1]\n\n![img](attachment://def)'),
      ],
      [],
      [],
      '',
    );
    // The second entry was placeholder-only, so it drops as blank.
    expect(entries).toEqual(['look at  please']);
  });

  it('caps the walk', () => {
    const rows = Array.from({ length: HISTORY_RECALL_LIMIT + 20 }, (_, i) =>
      row(`u${i}`, HISTORY_RECALL_LIMIT + 20 - i, `message ${i}`));
    const entries = buildRecallEntries(rows, [], ['pending'], '');
    expect(entries).toHaveLength(HISTORY_RECALL_LIMIT);
    expect(entries[0]).toBe('pending');
  });
});

interface HarnessOptions {
  rows?: RecallHistoryRow[];
  initialContent?: string;
  pendingSave?: boolean;
  flushClearsPendingSave?: boolean;
  hasAttachments?: boolean;
}

function makeHarness(options: HarnessOptions = {}) {
  const state = {
    threadId: 'thread-1' as string | null,
    content: options.initialContent ?? '',
    pendingSave: options.pendingSave ?? false,
    hasAttachments: options.hasAttachments ?? false,
    painted: [] as string[],
    carets: [] as string[],
  };
  let resolveFetch: ((rows: RecallHistoryRow[]) => void) | null = null;
  const deps: ComposerHistoryRecallDeps = {
    threadId: () => state.threadId,
    draftContent: () => state.content,
    draftHasPendingSave: () => state.pendingSave,
    draftHasAttachments: () => state.hasAttachments,
    flushDraft: vi.fn(async () => {
      if (options.flushClearsPendingSave !== false) state.pendingSave = false;
    }),
    fetchHistory: vi.fn(
      () => new Promise<RecallHistoryRow[]>((resolve) => { resolveFetch = resolve; }),
    ),
    paneItems: () => [],
    pendingMessages: () => [],
    paint: (text, caret) => {
      state.content = text;
      state.painted.push(text);
      state.carets.push(caret);
    },
    reportError: vi.fn(),
  };
  const recall = createComposerHistoryRecall(deps);
  return {
    recall,
    deps,
    state,
    async resolveFetch(rows = options.rows ?? []) {
      resolveFetch!(rows);
      await Promise.resolve();
      await Promise.resolve();
    },
  };
}

const twoRows = [row('u2', 1, 'second'), row('u1', 0, 'first')];

describe('createComposerHistoryRecall', () => {
  it('walks up through history and back down to the typed draft', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed' });

    expect(h.recall.arrowUp()).toBe(true);
    await h.resolveFetch();
    expect(h.state.content).toBe('second');

    expect(h.recall.arrowUp()).toBe(true);
    expect(h.state.content).toBe('first');

    // At the oldest entry the keystroke stays claimed and nothing moves.
    expect(h.recall.arrowUp()).toBe(true);
    expect(h.state.content).toBe('first');

    expect(h.recall.arrowDown()).toBe(true);
    expect(h.state.content).toBe('second');

    // Past the newest entry: the stash comes back and the session ends.
    expect(h.recall.arrowDown()).toBe(true);
    expect(h.state.content).toBe('typed');
    expect(h.recall.hasActiveSession()).toBe(false);

    // Nothing below the message being typed — unclaimed no-op.
    expect(h.recall.arrowDown()).toBe(false);
    expect(h.state.content).toBe('typed');

    // Every paint parked the caret at the walk's leading edge, so
    // repeating the same arrow keeps walking: offset 0 going up, the
    // end going down (stash restore included).
    expect(h.state.carets).toEqual(['start', 'start', 'end', 'end']);
  });

  it('arrowDown without a session is an unclaimed no-op', () => {
    const h = makeHarness();
    expect(h.recall.arrowDown()).toBe(false);
    expect(h.deps.fetchHistory).not.toHaveBeenCalled();
  });

  it('an empty history starts no session and paints nothing', async () => {
    const h = makeHarness({ rows: [], initialContent: 'typed' });
    expect(h.recall.arrowUp()).toBe(true);
    await h.resolveFetch();
    expect(h.state.painted).toEqual([]);
    expect(h.recall.hasActiveSession()).toBe(false);
  });

  it('an edit ends the session and becomes the next stash', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: '' });
    h.recall.arrowUp();
    await h.resolveFetch();
    expect(h.state.content).toBe('second');

    // The user edits the recalled text: setContent runs outside this
    // module, so all the session sees is content it did not paint.
    h.state.content = 'second, edited';
    expect(h.recall.hasActiveSession()).toBe(false);

    // The next walk starts fresh over the edited text as its stash.
    h.recall.arrowUp();
    await h.resolveFetch(twoRows);
    expect(h.state.content).toBe('second');
    h.recall.arrowDown();
    expect(h.state.content).toBe('second, edited');
  });

  it('a thread switch ends the session', async () => {
    const h = makeHarness({ rows: twoRows });
    h.recall.arrowUp();
    await h.resolveFetch();
    expect(h.state.content).toBe('second');
    h.state.threadId = 'thread-2';
    expect(h.recall.hasActiveSession()).toBe(false);
    expect(h.recall.arrowDown()).toBe(false);
  });

  it('flushes the typed draft before the first preview', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed', pendingSave: true });
    h.recall.arrowUp();
    await Promise.resolve();
    expect(h.deps.flushDraft).toHaveBeenCalledOnce();
    await h.resolveFetch();
    expect(h.state.content).toBe('second');
  });

  it('refuses to paint over a draft whose save failed', async () => {
    const h = makeHarness({
      rows: twoRows,
      initialContent: 'typed',
      pendingSave: true,
      flushClearsPendingSave: false,
    });
    h.recall.arrowUp();
    await Promise.resolve();
    await Promise.resolve();
    expect(h.deps.fetchHistory).not.toHaveBeenCalled();
    expect(h.state.content).toBe('typed');
  });

  it('drops a session start when the user typed while it fetched', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'ty' });
    h.recall.arrowUp();
    h.state.content = 'typed more';
    await h.resolveFetch();
    expect(h.state.painted).toEqual([]);
    expect(h.recall.hasActiveSession()).toBe(false);
  });

  it('swallows held-key repeats while the first start is fetching', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed' });
    expect(h.recall.arrowUp()).toBe(true);
    // Auto-repeat lands before the fetch answers: claimed, but no
    // second RPC and no second session start.
    expect(h.recall.arrowUp()).toBe(true);
    expect(h.deps.fetchHistory).toHaveBeenCalledOnce();
    await h.resolveFetch();
    expect(h.state.painted).toEqual(['second']);
    expect(h.state.content).toBe('second');
  });

  it('drops a session start when the thread switched while it fetched', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed' });
    h.recall.arrowUp();
    h.state.threadId = 'thread-2';
    await h.resolveFetch();
    expect(h.state.painted).toEqual([]);
    expect(h.recall.hasActiveSession()).toBe(false);
  });

  it('refuses to start over a draft holding attachments or chips', () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed', hasAttachments: true });
    // Unclaimed: the native (no-op) caret move keeps the key.
    expect(h.recall.arrowUp()).toBe(false);
    expect(h.deps.fetchHistory).not.toHaveBeenCalled();
  });

  it('an attachment landing mid-session ends it', async () => {
    const h = makeHarness({ rows: twoRows, initialContent: 'typed' });
    h.recall.arrowUp();
    await h.resolveFetch();
    expect(h.state.content).toBe('second');

    // An upload completing or a terminal send-to-composer queues a save
    // of what is on screen — the same takeover an edit is.
    h.state.hasAttachments = true;
    expect(h.recall.hasActiveSession()).toBe(false);
    expect(h.recall.arrowDown()).toBe(false);
    expect(h.recall.arrowUp()).toBe(false);
    expect(h.state.content).toBe('second');
  });

  it('surfaces a failed history read', async () => {
    const h = makeHarness();
    (h.deps.fetchHistory as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('boom'));
    h.recall.arrowUp();
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    expect(h.deps.reportError).toHaveBeenCalledOnce();
    expect(h.recall.hasActiveSession()).toBe(false);
  });
});

describe('recallArrowIntent', () => {
  const noMods = { shiftKey: false, ctrlKey: false, metaKey: false, altKey: false };
  const key = (k: string, mods: Partial<typeof noMods> = {}) => ({ key: k, ...noMods, ...mods });

  it('claims ArrowUp only from the very first position', () => {
    expect(recallArrowIntent(key('ArrowUp'), { start: 0, end: 0, valueLength: 5 })).toBe('up');
    expect(recallArrowIntent(key('ArrowUp'), { start: 2, end: 2, valueLength: 5 })).toBeNull();
  });

  it('claims ArrowDown only from the very last position', () => {
    expect(recallArrowIntent(key('ArrowDown'), { start: 5, end: 5, valueLength: 5 })).toBe('down');
    expect(recallArrowIntent(key('ArrowDown'), { start: 2, end: 2, valueLength: 5 })).toBeNull();
  });

  it('declines a non-collapsed selection even at the boundary', () => {
    expect(recallArrowIntent(key('ArrowUp'), { start: 0, end: 3, valueLength: 5 })).toBeNull();
    expect(recallArrowIntent(key('ArrowDown'), { start: 3, end: 5, valueLength: 5 })).toBeNull();
  });

  it('declines every modifier', () => {
    for (const mods of [{ shiftKey: true }, { ctrlKey: true }, { metaKey: true }, { altKey: true }]) {
      expect(recallArrowIntent(key('ArrowUp', mods), { start: 0, end: 0, valueLength: 5 })).toBeNull();
    }
  });

  it('declines other keys', () => {
    expect(recallArrowIntent(key('ArrowLeft'), { start: 0, end: 0, valueLength: 5 })).toBeNull();
    expect(recallArrowIntent(key('Enter'), { start: 0, end: 0, valueLength: 5 })).toBeNull();
  });
});
