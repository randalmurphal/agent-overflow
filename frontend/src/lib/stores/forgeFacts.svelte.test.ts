import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import {
  FACTS_PER_CALL,
  FACT_FRESH_MS,
  FACT_RETRY_MS,
  MAX_FACT_TEXT_PER_PR,
  MAX_FACTS_PER_PR,
  __forgeFactsHeldForTest,
  __forgeFactsKeysForTest,
  __resetForgeFactsForTest,
  __setResolveForgeFactsForTest,
  dropForgeFacts,
  forgeFactReader,
  forgeFactsCounters,
  noteForgeFactsRevision,
  noteForgeFactsWorkspace,
  observePRFacts,
  type ResolveForgeFactsCall,
} from './forgeFacts.svelte';
import { attachPR, applyPRUpdatedEvent } from './prReviewStore.svelte';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { __addFactScanForTest, type FactScan } from '../utils/forgeReferenceExtension';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { prKey, type PRRef } from '../utils/prReference';
import type { PRDetail, ReviewComment, ReviewThread } from '../types/models';
import { linesFactKey, type ForgeFact } from '../utils/forgeFacts';
import type { WorkspaceRef } from '../types/git';
import * as diagnostics from '../utils/frontendErrorCapture';

// The facts store's lifecycle, held to counts: what one applied snapshot
// scans and asks, what an answer changes, and what dropping the PR leaves.

const REF: PRRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 };
const KEY = prKey(REF);
const OTHER_REF: PRRef = { ...REF, number: 4 };
const OTHER = prKey(OTHER_REF);
const WS: WorkspaceRef = { projectId: 'p', workspacePath: '/w' };
const SHA = 'abc1234';

function detail(body: string): PRDetail {
  return {
    number: 3, title: 'MR', body, authorLogin: 'a', state: 'open', draft: false, baseRefName: 'main', baseRefSHA: 'b1',
    headRefName: 'f', headSHA: 'h1', url: 'https://gitlab.com/group/widget/-/merge_requests/3', additions: 0, deletions: 0,
    changedFiles: 0, viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: false, viewerCanApprove: false, viewerApproved: false },
  } as PRDetail;
}

function comment(id: string, body: string): ReviewComment {
  return { id, authorLogin: 'a', body, createdAt: id, url: '' } as unknown as ReviewComment;
}

function thread(id: string, comments: ReviewComment[]): ReviewThread {
  return { id, path: '', line: null, side: 'right', isResolvable: true, isResolved: false, isOutdated: false, comments } as unknown as ReviewThread;
}

const snapshot = (body: string, comments: ReviewComment[] = []) => ({ detail: detail(body), threads: comments.length ? [thread('t', comments)] : [] });

/** A resolver whose calls wait until the test answers them. */
function heldResolver() {
  const calls: { keys: readonly string[]; head: string; answer: (facts: ForgeFact[]) => void; reject: (err: unknown) => void }[] = [];
  const call: ResolveForgeFactsCall = (_backend, _ws, _pr, revision, keys) =>
    new Promise((resolve, reject) => calls.push({ keys: [...keys], head: revision?.headSHA ?? '', answer: (facts) => resolve({ facts }), reject }));
  __setResolveForgeFactsForTest(call);
  return calls;
}

const found = (key: string, extra: Partial<ForgeFact> = {}): ForgeFact => ({ key, status: 'found', url: 'https://gitlab.com/group/widget/-/issues/1', title: 'T', ...extra });

async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) await tick();
}

beforeEach(() => {
  __resetForgeFactsForTest();
});

afterEach(() => {
  vi.useRealTimers();
  __resetForgeFactsForTest();
});

describe('forge facts per applied snapshot', () => {
  it('marks unseen keys pending in the same task and asks for them in one call', () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('Fixes #1 and !2 by @ana', [comment('c1', `see ${SHA} and ~bug`)]));
    const reader = forgeFactReader(KEY)!;
    expect(reader.get('ref:group/widget#1')).toBe('pending');
    expect(reader.get(`commit:${SHA}`)).toBe('pending');
    expect(calls).toHaveLength(1);
    expect(new Set(calls[0].keys)).toEqual(new Set(['ref:group/widget#1', 'mr:group/widget!2', 'user:ana', `commit:${SHA}`, 'label:group/widget~bug']));
  });

  it('asks nothing again for N frames with the same bodies, and scans none of them', () => {
    const calls = heldResolver();
    for (let i = 0; i < 5; i++) observePRFacts(KEY, REF, snapshot('Fixes #1', [comment('c1', 'and #2')]));
    expect(calls).toHaveLength(1);
    expect(forgeFactsCounters.scans).toBe(2);
  });

  it('asks for exactly the one new key a new comment brings', () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('Fixes #1', [comment('c1', 'and #2')]));
    observePRFacts(KEY, REF, snapshot('Fixes #1', [comment('c1', 'and #2'), comment('c2', 'also #3 and #1')]));
    expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#1', 'ref:group/widget#2'], ['ref:group/widget#3']]);
    expect(forgeFactsCounters.scans).toBe(3);
  });

  it('makes no call and no request array for a frame without references', () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('plain words', [comment('c1', 'more words')]));
    observePRFacts(KEY, REF, snapshot('plain words, edited', [comment('c1', 'more words')]));
    expect(calls).toHaveLength(0);
    expect(forgeFactsCounters.requestArrays).toBe(0);
    expect(forgeFactsCounters.calls).toBe(0);
  });

  it('chunks a frame of more keys than a call takes', () => {
    const calls = heldResolver();
    const body = Array.from({ length: 200 }, (_, i) => `#${i + 1}`).join(' ');
    observePRFacts(KEY, REF, snapshot(body, [comment('c1', body.replace(/#/g, '!'))]));
    expect(calls.map((call) => call.keys.length)).toEqual([FACTS_PER_CALL, 400 - FACTS_PER_CALL]);
  });

  it('keeps at most MAX_FACTS_PER_PR facts for 10,000 distinct keys', () => {
    const calls = heldResolver();
    const comments = Array.from({ length: 50 }, (_, c) => comment(`c${c}`, Array.from({ length: 200 }, (_, i) => `#${c * 200 + i + 1}`).join(' ')));
    observePRFacts(KEY, REF, snapshot('', comments));
    expect(__forgeFactsHeldForTest(KEY)?.facts).toBe(MAX_FACTS_PER_PR);
    expect(calls.reduce((sum, call) => sum + call.keys.length, 0)).toBe(MAX_FACTS_PER_PR);
    // A key past the cap is not asked for and reads as never asked.
    expect(forgeFactReader(KEY)!.get('ref:group/widget#10000')).toBeUndefined();
  });

  it('drops the facts of a body that is gone, unless another body names them', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1', [comment('c1', '#1 #2'), comment('c2', '#3')]));
    calls[0].answer(calls[0].keys.map((key) => found(key)));
    await settle();
    observePRFacts(KEY, REF, snapshot('#1', [comment('c1', '#1')]));
    expect(__forgeFactsHeldForTest(KEY)).toMatchObject({ facts: 1, bodies: 2 });
  });
});

describe('answers', () => {
  it('update only the keys they name and bump the version once', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1 #2 #3'));
    const before = forgeFactReader(KEY)!;
    const version = __forgeFactsHeldForTest(KEY)!.version;
    calls[0].answer([found('ref:group/widget#1'), { key: 'ref:group/widget#2', status: 'absent' }]);
    await settle();
    const after = forgeFactReader(KEY)!;
    expect(__forgeFactsHeldForTest(KEY)!.version).toBe(version + 1);
    // The earlier snapshot still answers as it did: scopes compare the two.
    expect(before.get('ref:group/widget#1')).toBe('pending');
    expect(after.get('ref:group/widget#1')).toMatchObject({ status: 'found' });
    expect(after.signature('ref:group/widget#1')).not.toBe(before.signature('ref:group/widget#1'));
    // A key the answer left out could not be answered.
    expect(after.get('ref:group/widget#3')).toMatchObject({ status: 'unavailable' });
  });

  it('keeps a signature when an answer restates the same fact', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    calls[0].answer([found('ref:group/widget#1')]);
    await settle();
    const signature = forgeFactReader(KEY)!.signature('ref:group/widget#1');
    const version = __forgeFactsHeldForTest(KEY)!.version;
    vi.setSystemTime(Date.now() + FACT_FRESH_MS + 1);
    observePRFacts(KEY, REF, snapshot('#1 edited'));
    expect(calls).toHaveLength(2);
    // Meanwhile the old answer stays in use.
    expect(forgeFactReader(KEY)!.get('ref:group/widget#1')).toMatchObject({ status: 'found' });
    calls[1].answer([found('ref:group/widget#1')]);
    await settle();
    expect(forgeFactReader(KEY)!.signature('ref:group/widget#1')).toBe(signature);
    expect(__forgeFactsHeldForTest(KEY)!.version).toBe(version);
  });

  it('asks again for an unavailable key a minute after its answer, once per frame that still holds it', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1 #2'));
    calls[0].answer([{ key: 'ref:group/widget#1', status: 'unavailable' }, found('ref:group/widget#2')]);
    await settle();
    // Frames inside the minute ask nothing: a key nobody can answer costs
    // one request a minute at most, however often the PR changes.
    for (let i = 0; i < 5; i++) observePRFacts(KEY, REF, snapshot('#1 #2', [comment('c1', `early ${i}`)]));
    expect(calls).toHaveLength(1);
    vi.setSystemTime(Date.now() + FACT_RETRY_MS);
    observePRFacts(KEY, REF, snapshot('#1 #2', [comment('c1', 'x')]));
    observePRFacts(KEY, REF, snapshot('#1 #2', [comment('c1', 'y')]));
    // The second frame finds the first one's request still out.
    expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#1', 'ref:group/widget#2'], ['ref:group/widget#1']]);
  });

  it('asks at once for what was unavailable for want of a checkout, when one is attached', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    calls[0].answer([{ key: 'ref:group/widget#1', status: 'unavailable' }]);
    await settle();
    noteForgeFactsWorkspace(KEY, WS);
    observePRFacts(KEY, REF, snapshot('#1', [comment('c1', 'x')]));
    expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#1'], ['ref:group/widget#1']]);
  });

  it('asks again for a fact older than ten minutes, by the next frame that holds it', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    calls[0].answer([found('ref:group/widget#1')]);
    await settle();
    observePRFacts(KEY, REF, snapshot('#1', [comment('c1', 'x')]));
    expect(calls).toHaveLength(1);
    vi.setSystemTime(Date.now() + FACT_FRESH_MS);
    observePRFacts(KEY, REF, snapshot('#1', [comment('c1', 'y')]));
    expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#1'], ['ref:group/widget#1']]);
  });

  it('marks a rejected call\'s keys unavailable and reports kinds and counts only', async () => {
    const report = vi.spyOn(diagnostics, 'reportFrontendDiagnostic').mockImplementation(() => {});
    try {
      const calls = heldResolver();
      observePRFacts(KEY, REF, snapshot('#1 #2 @secret-person'));
      calls[0].reject(new Error('forge said no about @secret-person'));
      await settle();
      expect(forgeFactReader(KEY)!.get('user:secret-person')).toMatchObject({ status: 'unavailable' });
      expect(report).toHaveBeenCalledTimes(1);
      const [message, detail] = report.mock.calls[0];
      expect(message).toBe('forge facts: a request failed');
      expect(detail).toBe('ref 2, user 1; Error');
      expect(`${message}${detail}`).not.toContain('secret');
    } finally {
      report.mockRestore();
    }
  });

  it('reads an answer that names no key as every key unavailable, and asks again', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    vi.useFakeTimers({ toFake: ['Date'] });
    calls[0].answer([]);
    await settle();
    expect(forgeFactReader(KEY)!.get('ref:group/widget#1')).toMatchObject({ status: 'unavailable' });
    vi.setSystemTime(Date.now() + FACT_RETRY_MS);
    observePRFacts(KEY, REF, snapshot('#1 #2'));
    expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#1'], ['ref:group/widget#2', 'ref:group/widget#1']]);
  });

  it('applies nothing for a PR dropped while its call was out', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    dropForgeFacts(KEY);
    observePRFacts(KEY, REF, snapshot('#1'));
    calls[0].answer([found('ref:group/widget#1')]);
    await settle();
    expect(forgeFactReader(KEY)!.get('ref:group/widget#1')).toBe('pending');
  });
});

describe('the text a PR holds', () => {
  const GH: PRRef = { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 9 };
  const GH_KEY = prKey(GH);
  const COMMIT = 'a'.repeat(40);
  const link = (file: string) => `see https://github.com/acme/widget/blob/${COMMIT}/${file}#L1-L2 here`;
  const lines = (file: string) => linesFactKey(COMMIT, file, 1, 2);
  const text = (file: string, chars: number): ForgeFact => ({ key: lines(file), status: 'found', oid: 'b'.repeat(40), path: file, text: 'x'.repeat(chars) });
  // The scan also names the URL's commit, which a lex may read as a reference.
  const COMMIT_FACT: ForgeFact = { key: `commit:${COMMIT}`, status: 'found', oid: COMMIT, title: 'subject' };
  const asked = (calls: { keys: readonly string[] }[]) => calls.map((call) => call.keys.filter((key) => key.startsWith('lines:')));

  it('is bounded: an answer past the bound reads unavailable and is not asked for again until text is dropped', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    const half = MAX_FACT_TEXT_PER_PR / 2 + 1;
    observePRFacts(GH_KEY, GH, snapshot('', [comment('c1', link('a.ts')), comment('c2', link('b.ts'))]));
    expect(asked(calls)).toEqual([[lines('a.ts'), lines('b.ts')]]);
    calls[0].answer([COMMIT_FACT, text('a.ts', half), text('b.ts', half)]);
    await settle();
    expect(forgeFactReader(GH_KEY)!.get(lines('a.ts'))).toMatchObject({ status: 'found' });
    expect(forgeFactReader(GH_KEY)!.get(lines('b.ts'))).toEqual({ key: lines('b.ts'), status: 'unavailable' });
    expect(__forgeFactsHeldForTest(GH_KEY)!.textChars).toBe(half);
    // However long it waits, the turned-away answer is not asked for again.
    vi.setSystemTime(Date.now() + 5 * FACT_RETRY_MS);
    observePRFacts(GH_KEY, GH, snapshot('edited', [comment('c1', link('a.ts')), comment('c2', link('b.ts'))]));
    expect(calls).toHaveLength(1);
    // The body holding the first goes: its text is released and the other is asked for by that frame.
    observePRFacts(GH_KEY, GH, snapshot('edited', [comment('c2', link('b.ts'))]));
    expect(__forgeFactsHeldForTest(GH_KEY)!.textChars).toBe(0);
    expect(calls.map((call) => call.keys)).toEqual([[COMMIT_FACT.key, lines('a.ts'), lines('b.ts')], [lines('b.ts')]]);
    calls[1].answer([text('b.ts', half)]);
    await settle();
    expect(forgeFactReader(GH_KEY)!.get(lines('b.ts'))).toMatchObject({ status: 'found' });
    expect(__forgeFactsHeldForTest(GH_KEY)!.textChars).toBe(half);
  });

  it('counts a refreshed answer once', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = heldResolver();
    observePRFacts(GH_KEY, GH, snapshot(link('a.ts')));
    calls[0].answer([COMMIT_FACT, text('a.ts', 100)]);
    await settle();
    vi.setSystemTime(Date.now() + FACT_FRESH_MS);
    observePRFacts(GH_KEY, GH, snapshot(`${link('a.ts')} edited`));
    calls[1].answer([COMMIT_FACT, text('a.ts', 140)]);
    await settle();
    expect(__forgeFactsHeldForTest(GH_KEY)!.textChars).toBe(140);
  });
});

describe('a computer without ResolveForgeFacts', () => {
  // The binding answers null there (stores/bindings.ts): the raw method
  // answering null stands in for that answer through the real binding.
  async function learnOnce(): Promise<void> {
    noteForgeFactsWorkspace(KEY, WS);
    noteForgeFactsWorkspace(OTHER, WS);
    setBindingMock('ResolveForgeFacts', async () => null);
    observePRFacts(KEY, REF, snapshot('#1 and ~bug', [comment('c1', '@ana')]));
    expect(forgeFactReader(KEY)!.get('label:group/widget~bug')).toBe('pending');
    await settle();
  }

  it('is learned from the first answer: what it held is dropped, and nothing is marked or asked again', async () => {
    await learnOnce();
    const raw = getBindingMock('ResolveForgeFacts');
    expect(raw).toHaveBeenCalledTimes(1);
    const reader = forgeFactReader(KEY)!;
    for (const key of ['ref:group/widget#1', 'label:group/widget~bug', 'user:ana']) {
      expect(reader.get(key)).toBeUndefined();
      expect(reader.signature(key)).toBe('');
    }
    const { scans, calls, requestArrays } = forgeFactsCounters;
    // New bodies, edited ones and another PR on the same computer.
    observePRFacts(KEY, REF, snapshot('#1, #2 and ~bug', [comment('c1', '@ana'), comment('c2', '%v1')]));
    observePRFacts(OTHER, OTHER_REF, snapshot('#7 ~other'));
    noteForgeFactsRevision(KEY, WS, { baseRef: 'main', baseSHA: 'b1', headSHA: 'h1' });
    await settle();
    expect(raw).toHaveBeenCalledTimes(1);
    expect(forgeFactsCounters).toMatchObject({ scans, calls, requestArrays });
    expect(forgeFactReader(KEY)!.get('ref:group/widget#2')).toBeUndefined();
    expect(forgeFactReader(OTHER)!.get('label:group/widget~other')).toBeUndefined();
  });

  it('is asked again once its connection changes', async () => {
    await learnOnce();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    observePRFacts(KEY, REF, snapshot('#1 and ~bug', [comment('c1', '@ana')]));
    expect(getBindingMock('ResolveForgeFacts')).toHaveBeenCalledTimes(2);
    expect(forgeFactReader(KEY)!.get('ref:group/widget#1')).toBe('pending');
  });
});

describe('scans that read the head commit', () => {
  // A kind keyed by the frame's head commit, as repository images are.
  const imageScan: FactScan = {
    readsHead: true,
    scan: (body, _shapes, revision, add) => {
      for (const match of body.matchAll(/!\[\]\(([^)]+)\)/g)) add(`image:${revision.head}:${match[1]}`);
    },
  };
  const at = (head: string, body: string, comments: ReviewComment[] = []) => ({
    detail: { ...detail(body), headSHA: head },
    threads: comments.length ? [thread('t', comments)] : [],
  });

  it('scan a body with such keys again when the head moves, and no other body', () => {
    const remove = __addFactScanForTest(imageScan);
    try {
      const calls = heldResolver();
      const comments = [comment('c1', 'plain #1'), comment('c2', 'none at all')];
      observePRFacts(KEY, REF, at('h1', '![](a.png) and #2', comments));
      expect(forgeFactsCounters.scans).toBe(3);
      observePRFacts(KEY, REF, at('h1', '![](a.png) and #2', comments));
      expect(forgeFactsCounters.scans).toBe(3);
      observePRFacts(KEY, REF, at('h2', '![](a.png) and #2', comments));
      // The description alone: the comments found no head-keyed key.
      expect(forgeFactsCounters.scans).toBe(4);
      expect(calls.map((call) => call.keys)).toEqual([['ref:group/widget#2', 'image:h1:a.png', 'ref:group/widget#1'], ['image:h2:a.png']]);
      // The old head's key went with the scan that named it.
      expect(forgeFactReader(KEY)!.get('image:h1:a.png')).toBeUndefined();
      expect(forgeFactReader(KEY)!.get('image:h2:a.png')).toBe('pending');
    } finally {
      remove();
    }
  });

  it('cost a body without such keys nothing when the head moves', () => {
    heldResolver();
    const comments = [comment('c1', '#1 and abc1234')];
    observePRFacts(KEY, REF, at('h1', '#2 @ana', comments));
    const scans = forgeFactsCounters.scans;
    for (const head of ['h2', 'h3', 'h4']) observePRFacts(KEY, REF, at(head, '#2 @ana', comments));
    expect(forgeFactsCounters.scans).toBe(scans);
  });
});

describe('local facts and the loaded revision', () => {
  const revision = (head: string) => ({ baseRef: 'main', baseSHA: 'b1', headSHA: head });

  it('asks once for the local keys without an answer when the revision becomes known or its head moves', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot(`#1 ${SHA}`));
    calls[0].answer([found('ref:group/widget#1'), { key: `commit:${SHA}`, status: 'unavailable' }]);
    await settle();
    noteForgeFactsRevision(KEY, WS, revision('h1'));
    noteForgeFactsRevision(KEY, WS, revision('h1'));
    expect(calls.slice(1).map((call) => [call.keys, call.head])).toEqual([[[`commit:${SHA}`], 'h1']]);
    calls[1].answer([found(`commit:${SHA}`, { oid: SHA.padEnd(40, '0'), title: 'Subject' })]);
    await settle();
    noteForgeFactsRevision(KEY, WS, revision('h2'));
    // Found at one head stays found: a commit does not depend on it.
    expect(calls).toHaveLength(2);
  });

  it('asks again for a local key whose request is still out at an older revision', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot(SHA));
    noteForgeFactsRevision(KEY, WS, revision('h1'));
    expect(calls.map((call) => [call.keys, call.head])).toEqual([[[`commit:${SHA}`], ''], [[`commit:${SHA}`], 'h1']]);
    // The answer at the revision lands first; the late one without a
    // repository to read does not undo it.
    calls[1].answer([found(`commit:${SHA}`)]);
    await settle();
    calls[0].answer([{ key: `commit:${SHA}`, status: 'unavailable' }]);
    await settle();
    expect(forgeFactReader(KEY)!.get(`commit:${SHA}`)).toMatchObject({ status: 'found' });
  });

  it('carries the revision on the frames after it', () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('plain'));
    noteForgeFactsRevision(KEY, WS, revision('h1'));
    observePRFacts(KEY, REF, snapshot(`now ${SHA}`));
    expect(calls.map((call) => call.head)).toEqual(['h1']);
  });
});

describe('readers', () => {
  it('re-evaluate a derived reading one PR only when that PR\'s facts change', async () => {
    const calls = heldResolver();
    observePRFacts(KEY, REF, snapshot('#1'));
    observePRFacts(OTHER, OTHER_REF, snapshot('#2'));
    let mine = 0;
    let theirs = 0;
    const cleanup = $effect.root(() => {
      const a = $derived.by(() => (mine++, forgeFactReader(KEY)));
      const b = $derived.by(() => (theirs++, forgeFactReader(OTHER)));
      $effect(() => {
        void a;
        void b;
      });
    });
    try {
      flushSync();
      expect([mine, theirs]).toEqual([1, 1]);
      calls[0].answer([found('ref:group/widget#1')]);
      await settle();
      flushSync();
      expect([mine, theirs]).toEqual([2, 1]);
    } finally {
      cleanup();
    }
  });

  it('answer an empty reader before a PR\'s first snapshot and null without a key', () => {
    expect(forgeFactReader(null)).toBeNull();
    expect(forgeFactReader(KEY)!.get('ref:group/widget#1')).toBeUndefined();
    expect(forgeFactReader(KEY)!.signature('ref:group/widget#1')).toBe('');
  });
});

describe('retention with the PR store', () => {
  it('holds nothing for a PR once the PR store drops it', async () => {
    heldResolver();
    setBindingMock('SetPRUpdatesActive', async () => undefined);
    setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    const ref: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };
    const key = prKey(ref);
    const first = { ...detail('Fixes #1'), number: 5, url: 'https://github.com/owner/repo/pull/5' };
    setBindingMock('SubscribePRUpdates', async () => ({ id: 's', prKey: key, detail: first, threads: [], headSHA: 'h1', seq: 1 }));
    const held = attachPR(key, { ref });
    await settle();
    expect(__forgeFactsHeldForTest(key)).toMatchObject({ facts: 1, bodies: 1 });
    // A frame is folded in too.
    applyPRUpdatedEvent({ prKey: key, detail: { ...first, body: 'Fixes #1 and #2' }, threads: [], headSHA: 'h1', seq: 2 });
    expect(__forgeFactsHeldForTest(key)).toMatchObject({ facts: 2 });
    held.release();
    await settle();
    expect(__forgeFactsHeldForTest(key)).toBeNull();
    expect(__forgeFactsKeysForTest()).toEqual([]);
  });
});
