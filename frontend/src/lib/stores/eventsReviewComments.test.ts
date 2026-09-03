import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { applyReviewCommentsChanged } from './eventsReviewComments';
import {
  getPlanComments,
  refreshPlanComments,
  resetProposedPlanCacheForTests,
} from './proposedPlans.svelte';
import {
  getDiffReviewComments,
  refreshDiffReviewComments,
  resetDiffReviewCommentsForTest,
} from './diffReviewComments.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { DiffReviewComment, ProposedPlanComment } from '../types/models';

// Inline review comments used to persist and answer their own caller only: a
// comment written on one device never appeared on another until that pane
// reloaded. `review:comments-changed` names the SET that moved; each store
// re-reads it, and only where it is already holding it.

function planComment(id: string, body: string): ProposedPlanComment {
  return {
    id,
    threadId: 'thread-1',
    planItemId: 'plan-1',
    status: 'draft',
    startLine: 1,
    endLine: 1,
    selectedText: '',
    body,
    createdAt: 0,
    updatedAt: 0,
  };
}

function diffComment(id: string, body: string): DiffReviewComment {
  return {
    id,
    threadId: 'thread-1',
    scope: 'workspace',
    sourceKey: 'src-1',
    filePath: 'a.go',
    status: 'draft',
    side: 'new',
    selectedText: '',
    body,
    createdAt: 0,
    updatedAt: 0,
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await Promise.resolve();
}

beforeEach(() => {
  resetBindingMocks();
  resetProposedPlanCacheForTests();
  resetDiffReviewCommentsForTest();
});

afterEach(() => {
  resetProposedPlanCacheForTests();
  resetDiffReviewCommentsForTest();
});

describe('applyReviewCommentsChanged — plan comments', () => {
  it('re-reads a plan’s set this client is holding', async () => {
    let rows = [planComment('c1', 'first')];
    setBindingMock('ListProposedPlanComments', async () => rows);
    await refreshPlanComments('thread-1', 'plan-1');
    expect(getPlanComments('thread-1', 'plan-1').map((c) => c.body)).toEqual(['first']);

    rows = [planComment('c1', 'first'), planComment('c2', 'from the phone')];
    applyReviewCommentsChanged({ threadId: 'thread-1', planItemId: 'plan-1' });
    await settle();

    expect(getPlanComments('thread-1', 'plan-1').map((c) => c.body)).toEqual([
      'first',
      'from the phone',
    ]);
  });

  // Reacting unconditionally would make every client cache a comment list for
  // every plan anyone commented on, on a channel that is not thread-filtered.
  it('asks nothing for a plan this client never loaded', async () => {
    const list = setBindingMock('ListProposedPlanComments', async () => []);

    applyReviewCommentsChanged({ threadId: 'thread-1', planItemId: 'plan-unseen' });
    await settle();

    expect(list).not.toHaveBeenCalled();
  });
});

describe('applyReviewCommentsChanged — diff review comments', () => {
  it('re-reads a diff set this client is holding', async () => {
    let rows = [diffComment('d1', 'first')];
    setBindingMock('ListDiffReviewComments', async () => rows);
    await refreshDiffReviewComments('thread-1', 'workspace', 'src-1');
    expect(getDiffReviewComments('thread-1', 'workspace', 'src-1')).toHaveLength(1);

    rows = [];
    applyReviewCommentsChanged({ threadId: 'thread-1', scope: 'workspace', sourceKey: 'src-1' });
    await settle();

    // A delete is a delete-OR-resolve, which is exactly why the frame carries
    // no row: the re-read is what says what the set now holds.
    expect(getDiffReviewComments('thread-1', 'workspace', 'src-1')).toHaveLength(0);
  });

  it('asks nothing for a source this client never loaded', async () => {
    const list = setBindingMock('ListDiffReviewComments', async () => []);

    applyReviewCommentsChanged({ threadId: 'thread-1', scope: 'workspace', sourceKey: 'other' });
    await settle();

    expect(list).not.toHaveBeenCalled();
  });

  it('ignores a frame that names neither set', async () => {
    const plan = setBindingMock('ListProposedPlanComments', async () => []);
    const diff = setBindingMock('ListDiffReviewComments', async () => []);

    applyReviewCommentsChanged({ threadId: 'thread-1' });
    applyReviewCommentsChanged({ threadId: '', planItemId: 'plan-1' });
    await settle();

    expect(plan).not.toHaveBeenCalled();
    expect(diff).not.toHaveBeenCalled();
  });
});
