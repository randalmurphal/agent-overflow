import { describe, expect, it, beforeEach } from 'vitest';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from './composerDraft.svelte';
import type { Attachment } from '../types/attachment';
import { setBindingMock } from '../../test/mocks/bindings-app';

function installMocks(draft: {
  content: string;
  attachmentIds: string[];
  terminalChips: Array<{ id: string; label: string; preview: string; content: string; createdAt: number }>;
}) {
  setBindingMock('GetDraft', async (id: string) => ({
    threadId: id,
    content: draft.content,
    attachmentIds: draft.attachmentIds,
    terminalChips: draft.terminalChips,
    updatedAt: 1,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => [
    sampleAttachment('att-1'),
    sampleAttachment('att-2'),
  ]);
}

function sampleAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 100,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

describe('composerDraft store', () => {
  beforeEach(() => {
    resetComposerDraftSnapshotsForTest();
    installMocks({ content: '', attachmentIds: [], terminalChips: [] });
  });

  it('hydrates content and attachments from the backend on setThread', async () => {
    installMocks({
      content: 'hello',
      attachmentIds: ['att-1'],
      terminalChips: [
        { id: 'chip-1', label: 'sh', preview: '$ ls', content: '$ ls', createdAt: 0 },
      ],
    });
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    expect(store.content).toBe('hello [Image #1]');
    expect(store.attachments.map((a) => a.id)).toEqual(['att-1']);
    expect(store.terminalChips.map((c) => c.id)).toEqual(['chip-1']);
  });

  it('debounced setContent calls SaveDraft with the latest content', async () => {
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const store = createComposerDraftStore({ debounceMs: 5 });
    await store.setThread('thread-1');

    store.setContent('a');
    store.setContent('ab');
    store.setContent('abc');

    await new Promise((r) => setTimeout(r, 20));
    // With debouncing, only the final value should persist.
    expect(saveMock).toHaveBeenCalled();
    const last = saveMock.mock.calls[saveMock.mock.calls.length - 1];
    expect(last[1]).toBe('abc');
  });

  it('setContentAndAttachments queues one save with matching content and attachment ids', async () => {
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    store.setContentAndAttachments('[Image #1]', [sampleAttachment('att-new')]);
    await new Promise((r) => setTimeout(r, 10));
    const last = saveMock.mock.calls[saveMock.mock.calls.length - 1];
    expect(last[1]).toBe('[Image #1]');
    expect(last[2]).toContain('att-new');
  });

  it('removeAttachment drops it from the local list', async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    store.setContentAndAttachments('[Image #1]', [sampleAttachment('att-x')]);
    expect(store.attachments).toHaveLength(1);
    store.removeAttachment('att-x');
    expect(store.attachments).toHaveLength(0);
  });

  it('clearAfterSend resets state and calls ClearDraft', async () => {
    const clearMock = setBindingMock('ClearDraft', async () => {});
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    store.setContent('about to send');
    store.setContentAndAttachments('about to send [Image #1]', [sampleAttachment('att-1')]);
    await store.clearAfterSend();
    expect(store.content).toBe('');
    expect(store.attachments).toHaveLength(0);
    expect(clearMock).toHaveBeenCalledWith('thread-1');
  });

  it('composeOutgoingMessage appends chips and keeps visible image placeholders structured', async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    store.setContent('look at this:');
    store.addTerminalChip({
      id: 'chip-1',
      label: 'sh',
      preview: '$ ls',
      content: '$ ls\nREADME',
      createdAt: 0,
    });
    store.setContentAndAttachments('look at this: [Image #1]', [sampleAttachment('att-1')]);
    const outgoing = store.composeOutgoingMessage();
    expect(outgoing).toContain('look at this: [Image #1]');
    expect(outgoing).toContain('```terminal');
    expect(outgoing).toContain('README');
    expect(outgoing).not.toContain('attachment://att-1');
    expect(store.attachments.map((attachment) => attachment.id)).toEqual(['att-1']);
  });

  it('persists pending saves when switching threads', async () => {
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const store = createComposerDraftStore({ debounceMs: 50 });
    await store.setThread('thread-A');
    store.setContent('A-typed');

    await store.setThread('thread-B');

    expect(saveMock).toHaveBeenCalledWith('thread-A', 'A-typed', [], [], null);
  });

  it('switches local thread state immediately even when saving the previous draft is slow', async () => {
    let resolveSave: (() => void) | undefined;
    const saveStarted = new Promise<void>((resolve) => {
      setBindingMock('SaveDraft', async () => {
        resolve();
        await new Promise<void>((finish) => {
          resolveSave = finish;
        });
      });
    });
    const store = createComposerDraftStore({ debounceMs: 50 });
    await store.setThread('thread-A');
    store.setContent('A-typed');

    const switchResult = await Promise.race([
      store.setThread('thread-B').then(() => 'switched'),
      new Promise<'blocked'>((resolve) => setTimeout(() => resolve('blocked'), 50)),
    ]);
    await saveStarted;

    expect(switchResult).toBe('switched');
    expect(store.threadId).toBe('thread-B');
    expect(store.content).toBe('');

    resolveSave?.();
  });

  it('uses backend draft state after a local draft has saved cleanly', async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    const saveMock = setBindingMock('SaveDraft', async () => {});
    await store.setThread('thread-A');
    store.setContent('saved draft');
    await store.flush();
    expect(saveMock).toHaveBeenCalledWith('thread-A', 'saved draft', [], [], null);

    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'backend changed',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 2,
    }));

    await store.setThread(null);
    await store.setThread('thread-A');

    expect(store.content).toBe('backend changed');
  });

  it('reloadFromBackend discards an active local snapshot and hydrates the persisted draft', async () => {
    const store = createComposerDraftStore({ debounceMs: 50 });
    await store.setThread('thread-A');
    store.setContent('local stale draft');

    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'selected checkpoint prompt',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 2,
    }));

    await store.reloadFromBackend('thread-A');

    expect(store.content).toBe('selected checkpoint prompt');
    expect(store.hasPendingSave).toBe(false);
  });

  it('prepareForExternalDraftReplace waits for active saves and cancels queued stale saves', async () => {
    let resolveSave: (() => void) | undefined;
    let resolveSaveStarted: (() => void) | undefined;
    const saveStarted = new Promise<void>((resolve) => {
      resolveSaveStarted = resolve;
    });
    const blockingSave = new Promise<void>((resolve) => {
      resolveSave = resolve;
    });
    const saveCalls: string[] = [];
    setBindingMock('SaveDraft', (threadId: string, nextContent: string) => {
      if (threadId !== 'thread-A') {
        return Promise.resolve();
      }
      saveCalls.push(nextContent);
      resolveSaveStarted?.();
      return blockingSave;
    });

    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-A');
    store.setContent('in flight');
    const activeFlush = store.flush();
    await saveStarted;

    store.setContent('queued stale');
    const prepared = store.prepareForExternalDraftReplace('thread-A').then(() => 'done');
    await Promise.resolve();

    await expect(Promise.race([
      prepared,
      new Promise<'waiting'>((resolve) => setTimeout(() => resolve('waiting'), 0)),
    ])).resolves.toBe('waiting');

    resolveSave?.();
    await activeFlush;
    await expect(prepared).resolves.toBe('done');
    await new Promise((resolve) => setTimeout(resolve, 5));

    expect(saveCalls).toEqual(['in flight']);
    expect(store.hasPendingSave).toBe(false);
  });

  it('ignores stale hydrate responses from superseded thread switches', async () => {
    const pendingA: Array<(draft: {
      threadId: string;
      content: string;
      attachmentIds: string[];
      terminalChips: [];
      updatedAt: number;
    }) => void> = [];
    setBindingMock('GetDraft', async (id: string) => {
      if (id !== 'thread-A') {
        return {
          threadId: id,
          content: '',
          attachmentIds: [],
          terminalChips: [],
          updatedAt: 1,
        };
      }
      return await new Promise((resolve) => {
        pendingA.push(resolve);
      });
    });
    setBindingMock('ListAttachments', async () => []);
    const store = createComposerDraftStore({ debounceMs: 0 });

    const firstSwitch = store.setThread('thread-A');
    await store.setThread('thread-B');
    const secondSwitch = store.setThread('thread-A');

    pendingA[1]?.({
      threadId: 'thread-A',
      content: 'current A',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 2,
    });
    await secondSwitch;
    expect(store.content).toBe('current A');

    pendingA[0]?.({
      threadId: 'thread-A',
      content: 'stale A',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 1,
    });
    await firstSwitch;

    expect(store.content).toBe('current A');
  });

  it('restores a pending draft from memory before the debounce save completes', async () => {
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const firstStore = createComposerDraftStore({ debounceMs: 10_000 });
    await firstStore.setThread('thread-A');
    firstStore.setContent('fast switch draft');

    const secondStore = createComposerDraftStore({ debounceMs: 10_000 });
    await secondStore.setThread('thread-A');

    expect(secondStore.content).toBe('fast switch draft');
    expect(saveMock).not.toHaveBeenCalled();

    await firstStore.flushPending();
  });

  it('flush writes current state immediately (bypassing debounce)', async () => {
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const store = createComposerDraftStore({ debounceMs: 10_000 });
    await store.setThread('thread-1');
    store.setContent('unflushed');
    const preFlushCalls = saveMock.mock.calls.length;
    await store.flush();
    expect(saveMock.mock.calls.length).toBeGreaterThan(preFlushCalls);
  });

  it('exposes an `error` message when SaveDraft rejects', async () => {
    setBindingMock('SaveDraft', async () => {
      throw new Error('offline');
    });
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    store.setContent('boom');
    await new Promise((r) => setTimeout(r, 10));
    expect(store.error).toMatch(/offline/);
  });

  it('hasDraft reflects non-empty content / attachments / chips', async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    expect(store.hasDraft).toBe(false);
    store.setContent('hi');
    expect(store.hasDraft).toBe(true);
    store.setContent('');
    store.setContentAndAttachments('[Image #1]', [sampleAttachment('att-1')]);
    expect(store.hasDraft).toBe(true);
    store.removeAttachment('att-1');
    store.setContent('');
    expect(store.hasDraft).toBe(false);
  });

  // --- sourceProposedPlan lifecycle ---
  //
  // Drafts seeded by "Implement plan in new thread" persist a back-reference
  // to the source plan so the eventual send marks the original plan
  // Accepted. The store must hydrate it, persist it, surface it, clear it
  // on send, and restore it on a failed send.

  it('hydrates sourceProposedPlan from the backend draft and surfaces it', async () => {
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'PLEASE IMPLEMENT THIS PLAN:\n# Plan',
      attachmentIds: [],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src', itemId: 'plan-1', payloadId: 'pl-1' },
      updatedAt: 1,
    }));
    setBindingMock('ListAttachments', async () => []);

    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-impl');
    expect(store.sourceProposedPlan).toEqual({
      threadId: 'src',
      itemId: 'plan-1',
      payloadId: 'pl-1',
    });
  });

  it('clearAfterSend resets sourceProposedPlan so subsequent turns are regular turns', async () => {
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'seed',
      attachmentIds: [],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src', itemId: 'plan-1' },
      updatedAt: 1,
    }));
    setBindingMock('ListAttachments', async () => []);

    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-impl');
    expect(store.sourceProposedPlan).not.toBeNull();

    await store.clearAfterSend();
    expect(store.sourceProposedPlan).toBeNull();
  });

  it('restoreDraftFor preserves sourceProposedPlan from the snapshot', async () => {
    setBindingMock('ListAttachments', async () => []);

    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-impl');

    await store.restoreDraftFor('thread-impl', {
      content: 'seed',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src', itemId: 'plan-1' },
    });
    expect(store.sourceProposedPlan).toEqual({ threadId: 'src', itemId: 'plan-1' });
  });

  it('persists sourceProposedPlan via SaveDraft and resets on thread switch', async () => {
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'seed',
      attachmentIds: [],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src', itemId: 'plan-1' },
      updatedAt: 1,
    }));
    setBindingMock('ListAttachments', async () => []);
    const saveMock = setBindingMock('SaveDraft', async () => {});

    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-impl');
    store.setContent('typing more');
    await store.flush();

    const lastSaveArgs = saveMock.mock.calls.at(-1);
    expect(lastSaveArgs?.[4]).toMatchObject({ threadId: 'src', itemId: 'plan-1' });

    // Switching thread away resets the in-memory ref so the next pane's
    // composer doesn't inherit the previous one's link.
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: '',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 1,
    }));
    await store.setThread('thread-other');
    expect(store.sourceProposedPlan).toBeNull();
  });
});
