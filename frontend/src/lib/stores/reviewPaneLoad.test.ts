// Where a pr-scope diff request lands.
//
// The pr scope is the only diff source whose workspace ref can be the zero
// ref, and the `workspace` route reads the machine out of the ref's project
// id. A zero ref names nobody, so without a pin these calls resolve home:
// on a phone attached to two machines that is the wrong clone and the wrong
// forge credentials. The thread names the machine instead.

import { beforeEach, describe, expect, it } from 'vitest';

import { loadPatch, type DiffSubject } from './reviewPaneLoad';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { takePinnedBackend } from '../transport/backends';
import { NO_WORKSPACE_REF } from '../utils/workspaceKey';
import type { PRSnapshot } from './prReviewStore.svelte';
import type { PRRef } from '../utils/prReference';

/** The pr scope never reads it; the signature does. */
const NO_EDIT_DESIRE = { pinnedItemId: null, current: null };

const REMOTE = 'remote-machine';
const THREAD = 'thread-on-remote';

const REF: PRRef = { forge: 'github', namespace: 'acme', repo: 'widgets', number: 7 };

function snapshot(): PRSnapshot {
  return {
    detail: { baseRefName: 'main', headRefName: 'feature', headSHA: 'head-sha' } as PRSnapshot['detail'],
    threads: [],
    headSHA: 'head-sha',
  };
}

function subject(threadId: string | null): DiffSubject {
  return { workspace: NO_WORKSPACE_REF, threadId };
}

/** Every pr-scope binding, each recording the target armed at the moment it
 * was called. `takePinnedBackend` is what the real dispatch reads, so this
 * observes exactly the fact the transport would. */
function recordTargets(): { seen: Map<string, string | null> } {
  const seen = new Map<string, string | null>();
  setBindingMock('ListPRCommits', () => {
    seen.set('ListPRCommits', takePinnedBackend());
    return Promise.resolve([{ sha: 'commit-sha', subject: 'a commit' }]);
  });
  setBindingMock('GetPRCommitDiff', () => {
    seen.set('GetPRCommitDiff', takePinnedBackend());
    return Promise.resolve('commit patch');
  });
  setBindingMock('GetPRDiff', () => {
    seen.set('GetPRDiff', takePinnedBackend());
    return Promise.resolve('whole patch');
  });
  return { seen };
}

describe('loadPatch, pr scope', () => {
  beforeEach(() => {
    __resetEntityIndexForTest();
  });

  it('pins the whole-PR diff to the machine that owns the thread', async () => {
    noteThread(THREAD, REMOTE);
    const { seen } = recordTargets();

    const loaded = await loadPatch(subject(THREAD), 'pr', null, null, REF, snapshot(), NO_EDIT_DESIRE, false);

    expect(loaded.patchText).toBe('whole patch');
    expect(seen.get('ListPRCommits')).toBe(REMOTE);
    expect(seen.get('GetPRDiff')).toBe(REMOTE);
  });

  it('pins the per-commit diff to that same machine', async () => {
    noteThread(THREAD, REMOTE);
    const { seen } = recordTargets();

    const loaded = await loadPatch(
      subject(THREAD),
      'pr',
      null,
      'commit-sha',
      REF,
      snapshot(),
      NO_EDIT_DESIRE,
      false,
    );

    expect(loaded.patchText).toBe('commit patch');
    expect(seen.get('GetPRCommitDiff')).toBe(REMOTE);
  });

  it('arms nothing for a thread the index does not know, so the route decides', async () => {
    const { seen } = recordTargets();

    await loadPatch(subject('never-indexed'), 'pr', null, null, REF, snapshot(), NO_EDIT_DESIRE, false);

    expect(seen.get('ListPRCommits')).toBeNull();
    expect(seen.get('GetPRDiff')).toBeNull();
  });

  it('arms nothing for a draft placeholder with no thread at all', async () => {
    const { seen } = recordTargets();

    await loadPatch(subject(null), 'pr', null, null, REF, snapshot(), NO_EDIT_DESIRE, false);

    expect(seen.get('GetPRDiff')).toBeNull();
  });
});
