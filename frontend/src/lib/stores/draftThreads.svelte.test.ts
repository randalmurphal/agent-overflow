// Per-(project, mode) draft thread store. Tests verify the Map-copy-on-
// write pattern that drives Svelte's reactivity, plus the reverse-lookup
// path that composerSend relies on for promote-and-clear.

import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearProjectDraft,
  findDraftEntry,
  getAllDrafts,
  getProjectDraft,
  resetForTest,
  setProjectDraft,
} from './draftThreads.svelte';
import type { Thread } from '../types/models';

function makeThread(id: string, mode: 'chat' | 'design' = 'chat'): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode,
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
    expect(getProjectDraft('proj-1', 'chat')).toBeUndefined();
    expect(getProjectDraft('proj-1', 'design')).toBeUndefined();
  });

  it('setProjectDraft stores the draft thread by (project, mode)', () => {
    const t = makeThread('thread-a');
    setProjectDraft('proj-1', 'chat', t);
    expect(getProjectDraft('proj-1', 'chat')).toBe(t);
    expect(getAllDrafts().size).toBe(1);
  });

  it('setProjectDraft replaces an existing draft for the same (project, mode)', () => {
    setProjectDraft('proj-1', 'chat', makeThread('thread-a'));
    setProjectDraft('proj-1', 'chat', makeThread('thread-b'));
    expect(getProjectDraft('proj-1', 'chat')?.id).toBe('thread-b');
    expect(getAllDrafts().size).toBe(1);
  });

  it('chat and design drafts coexist for the same project', () => {
    const chat = makeThread('chat-thread', 'chat');
    const design = makeThread('design-thread', 'design');
    setProjectDraft('proj-1', 'chat', chat);
    setProjectDraft('proj-1', 'design', design);
    expect(getProjectDraft('proj-1', 'chat')?.id).toBe('chat-thread');
    expect(getProjectDraft('proj-1', 'design')?.id).toBe('design-thread');
    expect(getAllDrafts().size).toBe(2);
  });

  it('clearProjectDraft removes only the (project, mode) entry', () => {
    setProjectDraft('proj-1', 'chat', makeThread('chat-thread', 'chat'));
    setProjectDraft('proj-1', 'design', makeThread('design-thread', 'design'));
    clearProjectDraft('proj-1', 'chat');
    expect(getProjectDraft('proj-1', 'chat')).toBeUndefined();
    expect(getProjectDraft('proj-1', 'design')?.id).toBe('design-thread');
    expect(getAllDrafts().size).toBe(1);
  });

  it('clearProjectDraft is a no-op when there is no matching draft', () => {
    expect(() => clearProjectDraft('no-such-project', 'chat')).not.toThrow();
    expect(getAllDrafts().size).toBe(0);
  });

  it('findDraftEntry returns the (projectId, mode) pair for a known draft', () => {
    setProjectDraft('proj-1', 'chat', makeThread('thread-a', 'chat'));
    setProjectDraft('proj-2', 'design', makeThread('thread-b', 'design'));
    expect(findDraftEntry('thread-a')).toEqual({ projectId: 'proj-1', mode: 'chat' });
    expect(findDraftEntry('thread-b')).toEqual({ projectId: 'proj-2', mode: 'design' });
  });

  it('findDraftEntry returns undefined for an unknown thread', () => {
    setProjectDraft('proj-1', 'chat', makeThread('thread-a'));
    expect(findDraftEntry('thread-zzz')).toBeUndefined();
  });

  it('resetForTest wipes the store', () => {
    setProjectDraft('proj-1', 'chat', makeThread('thread-a'));
    setProjectDraft('proj-2', 'design', makeThread('thread-b'));
    resetForTest();
    expect(getAllDrafts().size).toBe(0);
  });
});
