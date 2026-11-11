import { describe, expect, it } from 'vitest';
import { createReviewFind, type ReviewFind, type ReviewFindHost, type ReviewFindSource } from './reviewFind.svelte';

// The review find store's contract (reviewFind.svelte.ts header) as a
// transition table: every ordered pair of operations, every triple with
// a deferred host reveal, and each ask whose deferred reveal settles
// before or after every other operation, from a query still scanning.
// The table keeps its own model of the published matches, from the
// source's scans and the contract's rule (a query publishes at each pause,
// any scan publishes its whole result when it completes, a superseded one
// nothing), and of the reading position (matches from n3 on). After each
// operation the published matches are the model's and `selected` changes
// only as the contract says; after the sequence settles, the index points
// at `selected`, a reveal happened only for an ask, at most once per ask,
// never after close, and did happen when its match survived and the
// content is stable.

interface Hit { identity: string }

const OPS = [
  'query', 'next', 'prev', 'refresh', 'keep', 'remove', 'publish', 'complete',
  'hostTrue', 'hostFalse', 'hostDeferred', 'settleTrue', 'settleFalse', 'close', 'reopen',
] as const;
type Op = typeof OPS[number];

interface Ask { id: number; target: string | null; calls: number; done: number; lastCall: Call | null }
interface Call { ask: Ask | null; identity: string; answer: 'true' | 'false' | 'deferred'; scansAtCall: number; settled?: boolean }

async function flush(): Promise<void> {
  for (let i = 0; i < 12; i += 1) await Promise.resolve();
}

function harness() {
  let content = ['n1', 'n2', 'n3', 'n4', 'n5', 'n6'];
  let added = 0;
  let scans = 0;
  const gates: (() => void)[] = [];
  const violations: string[] = [];
  const asks: Ask[] = [];
  let ask: Ask | null = null;
  let nextAnswer: Call['answer'] = 'true';
  const running: { call: Call; resolve: (done: boolean) => void }[] = [];
  let find!: ReviewFind<Hit>;

  // The model: the published matches, whether a completed scan published
  // them, and which scan is live and what kind (the operation that starts
  // a scan names it; a scan the store starts on its own is a recount).
  let published: string[] = [];
  let publishedComplete = false;
  let liveScan = -1;
  let liveRunning = false;
  let nextScanKind: 'query' | 'recount' = 'recount';
  // The last published set each identity was in: the survivor order.
  const seen = new Map<string, string[]>();
  const modelPublish = (list: string[], whole: boolean) => {
    published = list;
    publishedComplete = whole;
    for (const id of list) seen.set(id, list);
  };
  // The reader is at n3: a new query's first match is the first from n3 on.
  const below = (id: string) => Number(id.slice(1)) >= 3;

  const src: ReviewFindSource<Hit> = {
    remote: false,
    run: async (query, context) => {
      scans += 1;
      const id = scans;
      const kind = nextScanKind;
      nextScanKind = 'recount';
      liveScan = id;
      liveRunning = true;
      if (kind === 'query') modelPublish([], false);
      const all = content.filter((item) => item.includes(query));
      const found: string[] = [];
      for (let i = 0; i < all.length; i += 1) {
        if (i > 0 && i % 2 === 0) {
          if (liveScan === id && kind === 'query') modelPublish(found.slice(), false);
          if (!context.live()) return false;
          await new Promise<void>((resolve) => gates.push(resolve));
          if (!context.live()) return false;
        }
        context.emit({ identity: all[i] });
        found.push(all[i]);
      }
      if (liveScan === id) {
        modelPublish(found, true);
        liveRunning = false;
      }
      return false;
    },
  };
  const host: ReviewFindHost<Hit> = {
    paneId: () => 'pane-table',
    source: () => src,
    readingPosition: () => (hit) => below(hit.identity),
    reveal: (hit) => {
      const call: Call = { ask, identity: hit.identity, answer: nextAnswer, scansAtCall: scans };
      nextAnswer = 'true';
      if (!ask) violations.push(`reveal ${hit.identity} with no ask (after close)`);
      else {
        if (ask.done > 0) violations.push(`reveal ${hit.identity} after ask ${ask.id} completed`);
        if (find.selected !== hit.identity) violations.push(`reveal ${hit.identity} while ${find.selected} is selected`);
        if (ask.target === null) ask.target = hit.identity;
        else if (ask.target !== hit.identity) violations.push(`reveal ${hit.identity} for ask of ${ask.target}`);
        ask.calls += 1;
        ask.lastCall = call;
      }
      if (call.answer === 'true') {
        if (call.ask) call.ask.done += 1;
        return true;
      }
      if (call.answer === 'false') {
        // The host opened something to show it: the pane rescans.
        queueMicrotask(() => find.refresh());
        return false;
      }
      return new Promise<boolean>((resolve) => running.push({ call, resolve }));
    },
    paint: () => {},
  };
  find = createReviewFind(host, () => 'diff');

  function newAsk(): void {
    ask = { id: asks.length, target: null, calls: 0, done: 0, lastCall: null };
    asks.push(ask);
  }

  function settle(done: boolean): void {
    const next = running.shift();
    if (!next) return;
    next.call.settled = true;
    // A reveal finishing for a match no longer wanted does not scroll.
    const shows = done && find.wants(next.call.identity);
    if (shows && next.call.ask) next.call.ask.done += 1;
    if (!shows) next.call.answer = 'false';
    next.resolve(shows);
  }

  // `selected` after the step rule, over the model's published matches:
  // steps wrap once a completed scan published them.
  function stepped(delta: 1 | -1): string | null {
    const at = find.selected === null ? -1 : published.indexOf(find.selected);
    if (published.length === 0) return find.selected;
    let next = at < 0 ? 0 : at + delta;
    if (next < 0 || next >= published.length) {
      if (!publishedComplete) return find.selected;
      next = (next + published.length) % published.length;
    }
    return published[next];
  }

  // A new query's first match in the published matches: the first from
  // the reading position on, else the last once a scan completed.
  function firstChoice(): string | null {
    return published.find(below) ?? (publishedComplete ? (published.at(-1) ?? null) : null);
  }

  function survivor(gone: string): string | null {
    const before = seen.get(gone) ?? [];
    const at = before.indexOf(gone);
    for (let i = at + 1; i < before.length; i += 1) if (content.includes(before[i])) return before[i];
    for (let i = at - 1; i >= 0; i -= 1) if (content.includes(before[i])) return before[i];
    return null;
  }

  async function apply(op: Op): Promise<void> {
    const was = find.selected;
    // A query that has not chosen its first match yet.
    const choosing = ask !== null && ask.target === null && was === null;
    let expected: string | null = was;
    let stepAsk = false;
    switch (op) {
      case 'query':
        newAsk();
        nextScanKind = 'query';
        find.search('n');
        expected = null;
        break;
      case 'reopen':
        closeModel();
        find.clear();
        newAsk();
        nextScanKind = 'query';
        find.search('n');
        expected = null;
        break;
      case 'next':
      case 'prev': {
        expected = stepped(op === 'next' ? 1 : -1);
        const moves = published.length > 0 && expected !== was;
        if (moves || (published.length > 0 && find.result.current < 0)) {
          newAsk();
          stepAsk = true;
        }
        find[op === 'next' ? 'next' : 'previous']();
        break;
      }
      case 'refresh':
        find.refresh();
        break;
      case 'keep':
        added += 1;
        content = [...content, `n${100 + added}`];
        find.refresh();
        break;
      case 'remove':
        if (was !== null) content = content.filter((item) => item !== was);
        find.refresh();
        break;
      case 'publish':
        gates.shift()?.();
        break;
      case 'complete':
        while (gates.length > 0) {
          gates.shift()!();
          await flush();
        }
        break;
      case 'hostTrue':
        nextAnswer = 'true';
        break;
      case 'hostFalse':
        nextAnswer = 'false';
        break;
      case 'hostDeferred':
        nextAnswer = 'deferred';
        break;
      case 'settleTrue':
        settle(true);
        break;
      case 'settleFalse':
        settle(false);
        break;
      case 'close':
        ask = null;
        closeModel();
        find.clear();
        expected = null;
        break;
    }
    if (stepAsk && ask && find.selected !== expected) violations.push(`${op}: selected ${find.selected}, step rule says ${expected}`);
    const unchosen = ask !== null && expected === null && (op === 'query' || op === 'reopen' || choosing);
    await flush();
    check(op, expected, unchosen);
  }

  function closeModel(): void {
    liveScan = -1;
    liveRunning = false;
    published = [];
    publishedComplete = false;
  }

  // After an operation, the published matches are the model's, and
  // `selected` is what the operation made it, or what scan progress may
  // make that: a new query's first match, or the nearest survivor of a
  // match that is gone.
  function check(op: Op, expected: string | null, unchosen: boolean): void {
    const shown = find.hits.map((hit) => hit.identity);
    if (shown.join() !== published.join()) violations.push(`${op}: published ${shown.join()}, contract says ${published.join()}`);
    if (find.result.pending !== liveRunning) violations.push(`${op}: pending ${find.result.pending} with a scan ${liveRunning ? '' : 'not '}running`);
    const now = find.selected;
    const allowed = new Set<string | null>([expected]);
    if (unchosen) allowed.add(firstChoice());
    if (expected !== null && !content.includes(expected)) allowed.add(survivor(expected));
    // A query chooses its first match, which can be gone already too.
    // A copy: the loop adds to the set.
    const candidates = Array.from(allowed);
    for (const id of candidates) if (id !== null && !content.includes(id)) allowed.add(survivor(id));
    if (!allowed.has(now)) violations.push(`${op}: selected ${now}, contract allows ${[...allowed].join(' or ')}`);
  }

  async function quiesce(): Promise<void> {
    for (let round = 0; round < 20; round += 1) {
      while (gates.length > 0) {
        gates.shift()!();
        const choosing = ask !== null && ask.target === null && find.selected === null;
        await flush();
        check('publish', find.selected, choosing);
      }
      if (running.length > 0) {
        settle(true);
        await flush();
        check('settleTrue', find.selected, false);
        continue;
      }
      await flush();
      if (gates.length === 0 && running.length === 0) return;
    }
    violations.push('did not settle');
  }

  function verdict(): string[] {
    const out = [...violations];
    if (find.result.pending) out.push('still pending');
    const index = find.selected === null ? -1 : find.hits.findIndex((hit) => hit.identity === find.selected);
    if (find.result.current !== index) out.push(`current ${find.result.current}, selected is at ${index}`);
    if (find.selected !== null && index < 0) out.push(`selected ${find.selected} is not in the matches`);
    for (const each of asks) if (each.done > 1) out.push(`ask ${each.id} revealed ${each.done} times`);
    // A reveal is owed only for the selected match of an open ask.
    for (const id of new Set([...content, ...seen.keys()])) {
      if (find.wants(id) && (id !== find.selected || !ask)) out.push(`a reveal of ${id} is owed while ${find.selected} is selected`);
    }
    // The latest ask, still open, whose match survived: it was revealed,
    // unless the host's last answer was that it could not show it yet and
    // nothing has been rescanned since it was asked.
    if (ask && ask.target !== null && find.selected === ask.target && ask.done === 0) {
      const last = ask.lastCall;
      const excused = last !== null && last.answer === 'false' && last.settled === true && scans === last.scansAtCall;
      if (!excused) out.push(`ask ${ask.id} for ${ask.target} was never revealed`);
    }
    return out;
  }

  return {
    async setup() {
      newAsk();
      nextScanKind = 'query';
      find.search('n');
      await flush();
      check('query', null, true);
    },
    apply,
    quiesce,
    verdict,
  };
}

const SEQUENCES: Op[][] = [];
for (const a of OPS) for (const b of OPS) SEQUENCES.push([a, b]);
for (const a of OPS) for (const b of OPS) for (const c of OPS) {
  if (a === 'hostDeferred' || b === 'hostDeferred' || c === 'hostDeferred') SEQUENCES.push([a, b, c]);
}

// A deferred host reveal for each kind of ask, settled before and after
// every other operation.
for (const asked of ['query', 'next', 'prev', 'reopen'] as const) {
  for (const other of OPS) {
    for (const settled of ['settleTrue', 'settleFalse'] as const) {
      SEQUENCES.push(['hostDeferred', asked, other, settled], ['hostDeferred', asked, settled, other]);
      // The rescan the other operation starts runs to its end first.
      SEQUENCES.push(['hostDeferred', asked, other, 'complete', settled], ['hostDeferred', asked, settled, other, 'complete']);
    }
  }
}

// A recount started while a query's published prefix reaches past the
// selected match, then that match removed: its survivor is the next match
// the prefix published, not one before it.
for (const step of ['next', 'prev'] as const) {
  SEQUENCES.push(['publish', step, 'refresh', 'remove', 'refresh', 'complete']);
  SEQUENCES.push(['publish', 'publish', step, 'refresh', 'remove', 'refresh', 'complete']);
}

describe('review find transitions', () => {
  it.each(SEQUENCES.map((ops) => [ops.join(' > '), ops] as const))('%s', async (_name, ops) => {
    const h = harness();
    await h.setup();
    for (const op of ops) await h.apply(op);
    await h.quiesce();
    expect(h.verdict()).toEqual([]);
  });
});
