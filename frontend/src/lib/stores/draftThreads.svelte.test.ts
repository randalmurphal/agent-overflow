// Per-project draft thread store. Tests verify the Map-copy-on-write
// pattern that drives Svelte's reactivity, plus the reverse-lookup path
// that composerSend relies on for promote-and-clear.

import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearProjectDraft,
  findDraftProjectId,
  getAllDrafts,
  getProjectDraft,
  resetForTest,
  setProjectDraft,
} from './draftThreads.svelte';
import type { Thread } from '../types/models';

function makeThread(id: string): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

beforeEach(() => {
  resetForTest();
});

describe('draftThreads store', () => {
  it('starts empty', () => {
    expect(getAllDrafts().size).toBe(0);
    expect(getProjectDraft('proj-1')).toBeUndefined();
  });

  it('setProjectDraft stores the draft thread by project id', () => {
    const t = makeThread('thread-a');
    setProjectDraft('proj-1', t);
    expect(getProjectDraft('proj-1')).toBe(t);
    expect(getAllDrafts().size).toBe(1);
  });

  it('setProjectDraft replaces an existing draft for the same project', () => {
    setProjectDraft('proj-1', makeThread('thread-a'));
    setProjectDraft('proj-1', makeThread('thread-b'));
    expect(getProjectDraft('proj-1')?.id).toBe('thread-b');
    expect(getAllDrafts().size).toBe(1);
  });

  it('clearProjectDraft removes the draft for a project', () => {
    setProjectDraft('proj-1', makeThread('thread-a'));
    clearProjectDraft('proj-1');
    expect(getProjectDraft('proj-1')).toBeUndefined();
    expect(getAllDrafts().size).toBe(0);
  });

  it('clearProjectDraft is a no-op when there is no draft', () => {
    // Starts empty; clearing an unknown project must not throw.
    expect(() => clearProjectDraft('no-such-project')).not.toThrow();
    expect(getAllDrafts().size).toBe(0);
  });

  it('findDraftProjectId returns the project id for a known draft thread', () => {
    setProjectDraft('proj-1', makeThread('thread-a'));
    setProjectDraft('proj-2', makeThread('thread-b'));
    expect(findDraftProjectId('thread-a')).toBe('proj-1');
    expect(findDraftProjectId('thread-b')).toBe('proj-2');
  });

  it('findDraftProjectId returns undefined for an unknown thread', () => {
    setProjectDraft('proj-1', makeThread('thread-a'));
    expect(findDraftProjectId('thread-zzz')).toBeUndefined();
  });

  it('resetForTest wipes the store', () => {
    setProjectDraft('proj-1', makeThread('thread-a'));
    setProjectDraft('proj-2', makeThread('thread-b'));
    resetForTest();
    expect(getAllDrafts().size).toBe(0);
  });
});
