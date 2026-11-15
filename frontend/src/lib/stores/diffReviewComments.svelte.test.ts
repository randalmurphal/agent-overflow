// The two draft caches (a thread's sets, diffReviewComments.svelte.ts, and a
// PR's, prDiffReviewComments.svelte.ts) are bounded: past their size the
// least recently used set goes, and never one a surface holds, so a pane
// never shows a source without the drafts SQLite holds for it.

import { beforeEach, describe, expect, it } from 'vitest';
import {
  getDiffReviewComments,
  holdDiffReviewComments,
  refreshDiffReviewComments,
  resetDiffReviewCommentsForTest,
} from './diffReviewComments.svelte';
import {
  getPRDiffReviewComments,
  holdPRDiffReviewComments,
  prDraftsKey,
  refreshPRDiffReviewComments,
  resetPRDiffReviewCommentsForTest,
} from './prDiffReviewComments.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { DiffReviewComment } from '../types/models';

// Past this many sets the caches evict.
const CAP = 16;

function row(sourceKey: string): DiffReviewComment {
  return {
    id: `d-${sourceKey}`, threadId: 'thread-1', scope: 'workspace', sourceKey, filePath: 'a.go', status: 'draft',
    side: 'new', newLine: 1, selectedText: '', body: sourceKey, createdAt: 0, updatedAt: 0,
  };
}

const sources = (count: number, prefix = 's') => Array.from({ length: count }, (_, index) => `${prefix}${index}`);

beforeEach(() => {
  resetDiffReviewCommentsForTest();
  resetPRDiffReviewCommentsForTest();
  setBindingMock('ListDiffReviewComments', async (_thread: string, _scope: string, sourceKey: string) => [row(sourceKey)]);
  setBindingMock('ListPRDiffReviewComments', async (_pr: unknown, _scope: string, sourceKey: string) => [row(sourceKey)]);
});

const thread = {
  name: 'a thread\'s drafts',
  read: (sourceKey: string) => refreshDiffReviewComments('thread-1', 'workspace', sourceKey),
  shown: (sourceKey: string) => getDiffReviewComments('thread-1', 'workspace', sourceKey),
  hold: (sourceKey: string) => holdDiffReviewComments('thread-1', 'workspace', sourceKey),
};
const target = { backend: HOME_BACKEND, pr: { forge: 'github' as const, host: 'github.com', namespace: 'o', repo: 'r', number: 5 } };
const pr = {
  name: 'a PR\'s drafts',
  read: (sourceKey: string) => refreshPRDiffReviewComments(target, 'workspace', sourceKey),
  shown: (sourceKey: string) => getPRDiffReviewComments(prDraftsKey(target), 'workspace', sourceKey),
  hold: (sourceKey: string) => holdPRDiffReviewComments(prDraftsKey(target), 'workspace', sourceKey),
};

for (const cache of [thread, pr]) {
  describe(cache.name, () => {
    it('evicts the least recently read set, not the first one read', async () => {
      await cache.read('kept');
      for (const source of sources(CAP - 1)) await cache.read(source);
      // Read again: now the most recent.
      await cache.read('kept');
      await cache.read('one-more');
      expect(cache.shown('kept').map((comment) => comment.body)).toEqual(['kept']);
      expect(cache.shown('s0')).toEqual([]);
      expect(cache.shown('s1').map((comment) => comment.body)).toEqual(['s1']);
    });

    it('never evicts a held set, and lets it go once released', async () => {
      const release = cache.hold('shown');
      await cache.read('shown');
      for (const source of sources(CAP * 2)) await cache.read(source);
      expect(cache.shown('shown').map((comment) => comment.body)).toEqual(['shown']);
      release();
      for (const source of sources(CAP, 't')) await cache.read(source);
      expect(cache.shown('shown')).toEqual([]);
    });

    it('holds a set while any holder does', async () => {
      const first = cache.hold('shown');
      const second = cache.hold('shown');
      await cache.read('shown');
      first();
      for (const source of sources(CAP * 2)) await cache.read(source);
      expect(cache.shown('shown').map((comment) => comment.body)).toEqual(['shown']);
      second();
      second();
      for (const source of sources(CAP, 't')) await cache.read(source);
      expect(cache.shown('shown')).toEqual([]);
    });
  });
}
