import { describe, expect, it, beforeEach } from 'vitest';
import { createShipChangesState, _testing } from './shipChanges.svelte';
import type { GitStatus } from '../types/git';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    openPrUrl: undefined,
    openPrNumber: undefined,
    ...overrides,
  };
}

describe('createShipChangesState', () => {
  let store: ReturnType<typeof createShipChangesState>;

  beforeEach(() => {
    store = createShipChangesState();
  });

  describe('initial state', () => {
    it('starts idle with empty fields', () => {
      expect(store.phase).toBe('idle');
      expect(store.status).toBeNull();
      expect(store.threadId).toBeNull();
      expect(store.commitSubject).toBe('');
      expect(store.commitBody).toBe('');
      expect(store.commitSha).toBeNull();
      expect(store.prTitle).toBe('');
      expect(store.prBody).toBe('');
      expect(store.prUrl).toBeNull();
      expect(store.error).toBeNull();
      expect(store.finished).toBe(false);
    });

    it('exposes false can* gates until a phase is entered', () => {
      expect(store.canCommit).toBe(false);
      expect(store.canPush).toBe(false);
      expect(store.canCreatePR).toBe(false);
    });
  });

  describe('open() and close()', () => {
    it('open() stores the thread id and stays on idle until setStatus() arrives', () => {
      store.open('t-1');
      expect(store.threadId).toBe('t-1');
      expect(store.phase).toBe('idle');
    });

    it('open() wipes every previous field', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true }));
      store.setCommitSubject('wip');
      store.setCommitBody('draft');

      store.open('t-2');
      expect(store.threadId).toBe('t-2');
      expect(store.phase).toBe('idle');
      expect(store.status).toBeNull();
      expect(store.commitSubject).toBe('');
      expect(store.commitBody).toBe('');
    });

    it('close() returns the machine to idle', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true }));
      store.close();
      expect(store.phase).toBe('idle');
      expect(store.threadId).toBeNull();
    });
  });

  describe('auto-advance via setStatus', () => {
    it('starts at commit.review when there are uncommitted changes', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, aheadCount: 0 }));
      expect(store.phase).toBe('commit.review');
    });

    it('starts at push.review when the branch is ahead with no changes', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: false, aheadCount: 2 }));
      expect(store.phase).toBe('push.review');
    });

    it('starts at pr.review when there is an upstream but no open PR and no work', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: false, aheadCount: 0, hasUpstream: true }));
      expect(store.phase).toBe('pr.review');
    });

    it('starts at pr.done when an open PR already exists', () => {
      store.open('t-1');
      store.setStatus(
        status({ hasChanges: false, aheadCount: 0, hasUpstream: true, openPrUrl: 'https://x/1' }),
      );
      expect(store.phase).toBe('pr.done');
    });

    it('starts at pr.done when there is no upstream and no work (nothing to ship)', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: false, aheadCount: 0, hasUpstream: false }));
      expect(store.phase).toBe('pr.done');
    });

    it('does NOT auto-advance once we are inside a flow', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true }));
      expect(store.phase).toBe('commit.review');
      // A follow-up status update after, say, the user edited outside the
      // drawer should not rewind or jump the machine.
      store.setStatus(status({ hasChanges: false, aheadCount: 1 }));
      expect(store.phase).toBe('commit.review');
    });

    it('exposes the helper used to compute the initial phase', () => {
      expect(_testing.initialPhaseForStatus(status({ hasChanges: true }))).toBe('commit.review');
      expect(_testing.initialPhaseForStatus(status({ aheadCount: 1 }))).toBe('push.review');
      expect(_testing.initialPhaseForStatus(status())).toBe('pr.review');
      expect(
        _testing.initialPhaseForStatus(status({ openPrUrl: 'x' })),
      ).toBe('pr.done');
    });
  });

  describe('commit flow', () => {
    beforeEach(() => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true }));
    });

    it('canCommit is true only once the subject is non-empty', () => {
      expect(store.canCommit).toBe(false);
      store.setCommitSubject('  ');
      expect(store.canCommit).toBe(false);
      store.setCommitSubject('hello');
      expect(store.canCommit).toBe(true);
    });

    it('seedCommit() sets subject and body together', () => {
      store.seedCommit('Subject', 'Body text');
      expect(store.commitSubject).toBe('Subject');
      expect(store.commitBody).toBe('Body text');
    });

    it('beginCommit() throws when the subject is blank', () => {
      store.setCommitSubject('  ');
      expect(() => store.beginCommit()).toThrow(/empty subject/i);
      expect(store.phase).toBe('commit.review');
    });

    it('completeCommit() advances through commit.busy to push.review', () => {
      store.setCommitSubject('update');
      store.beginCommit();
      expect(store.phase).toBe('commit.busy');
      store.completeCommit('deadbeef');
      expect(store.phase).toBe('push.review');
      expect(store.commitSha).toBe('deadbeef');
      expect(store.canPush).toBe(true);
    });

    it('failCommit() lands on commit.error with the error message', () => {
      store.setCommitSubject('update');
      store.beginCommit();
      store.failCommit('hook rejected');
      expect(store.phase).toBe('commit.error');
      expect(store.error).toBe('hook rejected');
    });

    it('retry() after failCommit() goes back to commit.review with error cleared', () => {
      store.setCommitSubject('update');
      store.beginCommit();
      store.failCommit('hook rejected');
      store.retry();
      expect(store.phase).toBe('commit.review');
      expect(store.error).toBeNull();
      // subject/body survive so the user can edit and retry.
      expect(store.commitSubject).toBe('update');
    });

    it('skipCommit() jumps straight to push.review', () => {
      store.skipCommit();
      expect(store.phase).toBe('push.review');
      expect(store.commitSha).toBeNull();
    });

    it('beginCommit() from a non-review phase throws', () => {
      store.setCommitSubject('x');
      store.beginCommit();
      expect(() => store.beginCommit()).toThrow(/illegal transition/i);
    });

    it('completeCommit() from a non-busy phase throws', () => {
      expect(() => store.completeCommit('sha')).toThrow(/illegal transition/i);
    });
  });

  describe('push flow', () => {
    beforeEach(() => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: false, aheadCount: 2 }));
    });

    it('start phase is push.review', () => {
      expect(store.phase).toBe('push.review');
      expect(store.canPush).toBe(true);
    });

    it('beginPush() advances to push.busy', () => {
      store.beginPush();
      expect(store.phase).toBe('push.busy');
      expect(store.canPush).toBe(false);
    });

    it('completePush() advances through push.done to pr.review', () => {
      store.beginPush();
      store.completePush();
      expect(store.phase).toBe('pr.review');
    });

    it('failPush() lands on push.error and retry() returns to push.review', () => {
      store.beginPush();
      store.failPush('auth required');
      expect(store.phase).toBe('push.error');
      expect(store.error).toBe('auth required');
      store.retry();
      expect(store.phase).toBe('push.review');
      expect(store.error).toBeNull();
    });

    it('skipPush() jumps to pr.review when the user wants to open a PR directly', () => {
      store.skipPush();
      expect(store.phase).toBe('pr.review');
    });

    it('beginPush() from pr.review throws', () => {
      store.skipPush();
      expect(() => store.beginPush()).toThrow(/illegal transition/i);
    });

    it('completePush() from push.review throws', () => {
      expect(() => store.completePush()).toThrow(/illegal transition/i);
    });
  });

  describe('pr flow', () => {
    beforeEach(() => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: false, aheadCount: 0, hasUpstream: true }));
    });

    it('canCreatePR is true only once the title is non-empty', () => {
      expect(store.canCreatePR).toBe(false);
      store.setPRTitle('  ');
      expect(store.canCreatePR).toBe(false);
      store.setPRTitle('Open PR');
      expect(store.canCreatePR).toBe(true);
    });

    it('seedPR() sets title and body together', () => {
      store.seedPR('PR Title', 'PR Body');
      expect(store.prTitle).toBe('PR Title');
      expect(store.prBody).toBe('PR Body');
    });

    it('beginCreatePR() throws when the title is blank', () => {
      store.setPRTitle('   ');
      expect(() => store.beginCreatePR()).toThrow(/title is required/i);
      expect(store.phase).toBe('pr.review');
    });

    it('completeCreatePR() stores URL and marks finished', () => {
      store.setPRTitle('Feature');
      store.beginCreatePR();
      expect(store.phase).toBe('pr.busy');
      store.completeCreatePR('https://github.com/o/r/pull/1');
      expect(store.phase).toBe('pr.done');
      expect(store.prUrl).toBe('https://github.com/o/r/pull/1');
      expect(store.finished).toBe(true);
    });

    it('failCreatePR() lands on pr.error and retry returns to pr.review', () => {
      store.setPRTitle('Feature');
      store.beginCreatePR();
      store.failCreatePR('gh cli missing');
      expect(store.phase).toBe('pr.error');
      expect(store.error).toBe('gh cli missing');
      store.retry();
      expect(store.phase).toBe('pr.review');
      expect(store.error).toBeNull();
      expect(store.prTitle).toBe('Feature');
    });

    it('beginCreatePR() from pr.done throws', () => {
      store.setPRTitle('Feature');
      store.beginCreatePR();
      store.completeCreatePR('url');
      expect(() => store.beginCreatePR()).toThrow(/illegal transition/i);
    });
  });

  describe('retry guards', () => {
    it('throws when called outside an error state', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true }));
      expect(() => store.retry()).toThrow(/retry/i);
    });
  });

  describe('full happy path', () => {
    it('walks commit -> push -> PR end to end', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, aheadCount: 0 }));

      // Commit
      store.seedCommit('Ship widget', 'Adds the new widget');
      expect(store.canCommit).toBe(true);
      store.beginCommit();
      store.completeCommit('sha-1');
      expect(store.phase).toBe('push.review');
      expect(store.commitSha).toBe('sha-1');

      // Push
      store.beginPush();
      store.completePush();
      expect(store.phase).toBe('pr.review');

      // PR
      store.seedPR('Ship widget', 'Adds the new widget');
      store.beginCreatePR();
      store.completeCreatePR('https://github.com/o/r/pull/42');
      expect(store.finished).toBe(true);
      expect(store.prUrl).toBe('https://github.com/o/r/pull/42');
    });
  });

  describe('pending operation gating (Bug C3)', () => {
    it('canCommit is false while a merge is in progress', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, pendingOperation: 'merge' }));
      store.setCommitSubject('try to commit');
      expect(store.canCommit).toBe(false);
    });

    it('canCommit is false while a rebase is in progress', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, pendingOperation: 'rebase' }));
      store.setCommitSubject('try to commit');
      expect(store.canCommit).toBe(false);
    });

    it('canCommit is false while a bisect is in progress', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, pendingOperation: 'bisect' }));
      store.setCommitSubject('try to commit');
      expect(store.canCommit).toBe(false);
    });

    it('canCommit is true when pendingOperation clears', () => {
      store.open('t-1');
      store.setStatus(status({ hasChanges: true, pendingOperation: 'merge' }));
      store.setCommitSubject('try to commit');
      expect(store.canCommit).toBe(false);
      // A fresh status snapshot with no pending op doesn't auto-advance but
      // does update the gate — canCommit returns true again.
      store.setStatus(status({ hasChanges: true, pendingOperation: '' }));
      expect(store.canCommit).toBe(true);
    });
  });
});
