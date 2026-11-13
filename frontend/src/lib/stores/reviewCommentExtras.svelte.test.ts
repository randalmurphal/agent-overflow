// A review pane's reactions and suggestion applies
// (reviewForgeOps.svelte.ts): nothing is optimistic, so a chip or a
// suggestion reads as pending until the PR's applied sequence reaches the
// one its write answered with; one operation per chip and per suggestion;
// each keyed by its PR, comment and name or index; a failure shows on its
// surface while the state lives, as a held toast once it is disposed, and
// nowhere once the pane's PR became another, which also closes the
// reaction popover.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { createReviewCommentExtras, type ReviewCommentExtras } from './reviewCommentExtras.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import type { CommentSuggestion, ReviewComment, ReviewThread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';

const REF = { forge: 'github' as const, host: 'github.com', namespace: 'o', repo: 'r', number: 5 };
const REF_B = { ...REF, number: 6 };

const COMMENT: ReviewComment = { id: 'c1', authorLogin: 'octo', body: 'b', createdAt: '2026-01-01T00:00:00Z', databaseID: 1, viewerCanEdit: false, viewerCanDelete: false, reactions: [{ name: 'thumbsup', count: 1, viewerReacted: false }] };
const OTHER: ReviewComment = { ...COMMENT, id: 'c2' };
const THREAD: ReviewThread = { id: 't1', path: 'src/a.ts', line: 3, side: 'right', isResolvable: true, isResolved: false, isOutdated: false, comments: [COMMENT, OTHER] };
const SUGGESTION: CommentSuggestion = { index: 0, to: ['x'], appliable: true, bodyStartLine: 0, bodyEndLine: 2, bodyPlaced: true, digest: 'a'.repeat(64) };

function hello(capable: boolean): void {
  __setTransportHelloForTest({
    launchId: 'L1', protocolVersion: 1, capabilities: capable ? ['pr-suggestions-reactions.v1'] : [],
    backendId: '', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  flushSync();
}

function makeHost() {
  const made = $state({
    ref: REF as typeof REF | null, appliedSeq: 1, launch: 'L1', failing: false,
    withheld: null as string | null, unavailable: null as string | null, refreshes: 0,
  });
  return made;
}

let host: ReturnType<typeof makeHost>;
let extras: ReviewCommentExtras;
let stop: () => void;

beforeEach(() => {
  resetToastsForTest();
  hello(true);
  host = makeHost();
  stop = $effect.root(() => {
    extras = createReviewCommentExtras({
      backend: HOME_BACKEND,
      ref: () => host.ref,
      identity: () => (host.ref ? `home:${host.ref.number}` : null),
      appliedSeq: () => host.appliedSeq,
      launch: () => host.launch,
      updateFailing: () => host.failing,
      refreshThreads: async () => { host.refreshes += 1; },
      onPR: () => host.ref !== null,
      withheld: () => host.withheld,
      loadedHeadSHA: () => 'head1',
      diffIsCurrentHead: () => true,
      files: () => [],
      openEditorKeys: () => new Set(),
      reactionsUnavailable: () => host.unavailable,
    });
  });
  flushSync();
});

afterEach(() => {
  extras.dispose();
  stop();
  __setTransportHelloForTest(null);
});

describe('a reaction chip', () => {
  it('is pending until the applied sequence reaches its answer', async () => {
    const call = setBindingMock('SetPRCommentReaction', async () => ({ seq: 3 }));
    const { reactions } = extras.state;
    await reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    expect(call).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { threadId: 't1', commentId: 'c1' }, 'thumbsup', true);
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(true);
    host.appliedSeq = 2;
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(true);
    host.appliedSeq = 3;
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(false);
  });

  it('gives the wait up with a new launch or a failing pump, and re-reads on seq 0', async () => {
    setBindingMock('SetPRCommentReaction', async () => ({ seq: 9 }));
    const { reactions } = extras.state;
    await reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    host.launch = 'L2';
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(false);
    host.launch = 'L1';
    host.failing = true;
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(false);
    setBindingMock('SetPRCommentReaction', async () => ({ seq: 0 }));
    await reactions.toggle(THREAD, COMMENT, 'heart', true);
    expect(reactions.pending(THREAD, COMMENT, 'heart')).toBe(false);
    expect(host.refreshes).toBe(1);
  });

  it('runs one write at a time per chip, keyed by comment and name', async () => {
    const answer = deferred<{ seq: number }>();
    const call = setBindingMock('SetPRCommentReaction', () => answer.promise);
    const { reactions } = extras.state;
    const first = reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    void reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    expect(call).toHaveBeenCalledTimes(1);
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(true);
    expect(reactions.pending(THREAD, COMMENT, 'heart')).toBe(false);
    expect(reactions.pending(THREAD, OTHER, 'thumbsup')).toBe(false);
    void reactions.toggle(THREAD, OTHER, 'thumbsup', true);
    expect(call).toHaveBeenCalledTimes(2);
    answer.resolve({ seq: 0 });
    await first;
    expect(reactions.pending(THREAD, COMMENT, 'thumbsup')).toBe(false);
  });

  it('shows a failure on its comment\'s chip row, the newest press owning it', async () => {
    const older = deferred<{ seq: number }>();
    setBindingMock('SetPRCommentReaction', () => older.promise);
    const { reactions } = extras.state;
    const first = reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    setBindingMock('SetPRCommentReaction', async () => { throw new TransportError('forge_refused', 'GitHub refused the reaction.'); });
    await reactions.toggle(THREAD, COMMENT, 'heart', false);
    expect(reactions.error(THREAD, COMMENT)).toBe('GitHub refused the reaction.');
    expect(reactions.error(THREAD, OTHER)).toBeNull();
    older.reject(new TransportError('forge_refused', 'An older refusal.'));
    await first;
    expect(reactions.error(THREAD, COMMENT)).toBe('GitHub refused the reaction.');
  });

  it('reads an unconfirmed write as not confirmed', async () => {
    setBindingMock('SetPRCommentReaction', async () => { throw new TransportError('forge_unconfirmed', 'GitHub did not confirm the request.'); });
    const { reactions } = extras.state;
    await reactions.toggle(THREAD, COMMENT, 'heart', true);
    expect(reactions.error(THREAD, COMMENT)).toMatch(/not confirm/);
  });

  it('writes nothing for a held key', async () => {
    const { blockHeldActivation } = await import('../utils/heldKey');
    const held = new KeyboardEvent('keydown', { key: 'Enter', repeat: true, cancelable: true });
    blockHeldActivation(held);
    expect(held.defaultPrevented).toBe(true);
    const fresh = new KeyboardEvent('keydown', { key: 'Enter', cancelable: true });
    blockHeldActivation(fresh);
    expect(fresh.defaultPrevented).toBe(false);
  });

  it('drops a late failure once the pane\'s PR became another, and closes the popover', async () => {
    const answer = deferred<{ seq: number }>();
    setBindingMock('SetPRCommentReaction', () => answer.promise);
    const { reactions } = extras.state;
    reactions.setPickerOpen(THREAD, COMMENT, true);
    expect(reactions.pickerOpen(THREAD, COMMENT)).toBe(true);
    const done = reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    host.ref = REF_B;
    flushSync();
    expect(reactions.pickerOpen(THREAD, COMMENT)).toBe(false);
    // Back on the first PR before the answer: it still started on a PR
    // the pane left, so it writes nothing.
    host.ref = REF;
    flushSync();
    answer.reject(new TransportError('forge_refused', 'Too late.'));
    await done;
    expect(reactions.error(THREAD, COMMENT)).toBeNull();
    expect(getToasts()).toHaveLength(0);
  });

  it('toasts a failure that answers after the state was disposed, with no Copy text', async () => {
    const answer = deferred<{ seq: number }>();
    setBindingMock('SetPRCommentReaction', () => answer.promise);
    const done = extras.state.reactions.toggle(THREAD, COMMENT, 'thumbsup', true);
    extras.dispose();
    answer.reject(new TransportError('forge_refused', 'Refused.'));
    await done;
    await settle();
    const toasts = getToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0].message).toMatch(/reaction on .* was not added/);
    expect(toasts[0].duration).toBe(0);
    expect(toasts[0].action).toBeUndefined();
  });

  it('shows no chips without the capability, and no Add reaction while the read is unavailable', () => {
    const { reactions } = extras.state;
    expect(reactions.reactionsOf(COMMENT)).toHaveLength(1);
    expect(reactions.canAdd).toBe(true);
    host.unavailable = 'GitLab did not answer the reactions read.';
    expect(reactions.canAdd).toBe(false);
    expect(extras.state.reactionsUnavailable).toBe('GitLab did not answer the reactions read.');
    hello(false);
    expect(reactions.available).toBe(false);
    expect(reactions.reactionsOf(COMMENT)).toHaveLength(0);
    expect(extras.state.reactionsUnavailable).toBeNull();
  });
});

describe('a suggestion\'s Apply', () => {
  it('names the loaded head and the suggestion, and waits for the PR frame', async () => {
    const call = setBindingMock('ApplyPRSuggestion', async () => ({ seq: 4 }));
    const { suggestions } = extras.state;
    await suggestions.apply(THREAD, COMMENT, SUGGESTION);
    expect(call).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), expect.objectContaining({
      target: { threadId: 't1', commentId: 'c1' }, index: 0, headSha: 'head1', digest: 'a'.repeat(64),
    }));
    expect(suggestions.pending(THREAD, COMMENT, SUGGESTION)).toBe(true);
    expect(suggestions.pending(THREAD, COMMENT, { index: 1 })).toBe(false);
    void suggestions.apply(THREAD, COMMENT, SUGGESTION);
    expect(call).toHaveBeenCalledTimes(1);
    host.appliedSeq = 4;
    expect(suggestions.pending(THREAD, COMMENT, SUGGESTION)).toBe(false);
  });

  it('reads the PR again for a refusal that says the suggestion changed, by its code', async () => {
    setBindingMock('ApplyPRSuggestion', async () => {
      throw new TransportError('forge_suggestion_changed', 'The suggestion changed since it was loaded. Reload the pull request.');
    });
    const { suggestions } = extras.state;
    await suggestions.apply(THREAD, COMMENT, SUGGESTION);
    expect(suggestions.error(THREAD, COMMENT, SUGGESTION)).toBe('The suggestion changed since it was loaded. Reload the pull request.');
    expect(suggestions.error(THREAD, COMMENT, { index: 1 })).toBeNull();
    expect(host.refreshes).toBe(1);
  });

  it('shows any other refusal under the suggestion without reading the PR again', async () => {
    setBindingMock('ApplyPRSuggestion', async () => {
      throw new TransportError('forge_refused', 'The suggestion was not applied: the file is a symbolic link.');
    });
    const { suggestions } = extras.state;
    await suggestions.apply(THREAD, COMMENT, SUGGESTION);
    expect(suggestions.error(THREAD, COMMENT, SUGGESTION)).toBe('The suggestion was not applied: the file is a symbolic link.');
    expect(host.refreshes).toBe(0);
  });

  it('does nothing with a refusal that answers after the pane\'s PR changed', async () => {
    const answer = deferred<{ seq: number }>();
    setBindingMock('ApplyPRSuggestion', () => answer.promise);
    const { suggestions } = extras.state;
    const done = suggestions.apply(THREAD, COMMENT, SUGGESTION);
    host.ref = REF_B;
    flushSync();
    answer.reject(new TransportError('forge_suggestion_changed', 'Too late.'));
    await done;
    expect(host.refreshes).toBe(0);
    expect(getToasts()).toHaveLength(0);
  });

  it('does not call for an applied, unappliable or withheld suggestion', async () => {
    const call = setBindingMock('ApplyPRSuggestion', async () => ({ seq: 0 }));
    const { suggestions } = extras.state;
    await suggestions.apply(THREAD, COMMENT, { ...SUGGESTION, applied: true });
    await suggestions.apply(THREAD, COMMENT, { ...SUGGESTION, appliable: false });
    host.withheld = 'The diff is not the live head.';
    await suggestions.apply(THREAD, COMMENT, SUGGESTION);
    expect(call).not.toHaveBeenCalled();
  });
});
