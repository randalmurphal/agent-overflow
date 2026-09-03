// `persistence: 'none'` — the store shape without any of its persistence.
//
// The editing surfaces that use it (an in-place message editor) sit on a
// thread whose real composer holds the user's actual draft, so the tests
// here assert NON-calls as hard as they assert behaviour: one SaveDraft or
// one registry write from a local store overwrites work the user can see.

import { describe, expect, it, beforeEach } from 'vitest';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from './composerDraft.svelte';
import { getRememberedDraftSnapshot } from './composerDraftSnapshots';
import type { Attachment } from '../types/attachment';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import { setBindingMock } from '../../test/mocks/bindings-app';

function sampleAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 100,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
    kind: 'image',
  };
}

function snapshot(overrides: Partial<ComposerDraftSnapshot> = {}): ComposerDraftSnapshot {
  return {
    content: 'seeded text',
    attachments: [],
    terminalChips: [],
    sourceProposedPlan: null,
    ...overrides,
  };
}

function localStore() {
  return createComposerDraftStore({ debounceMs: 0, persistence: 'none' });
}

/** Every RPC a draft store can reach. None of them may fire in local mode. */
function draftBindings() {
  return {
    save: setBindingMock('SaveDraft', async () => {}),
    get: setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'backend content',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 1,
    })),
    clear: setBindingMock('ClearDraft', async () => {}),
    list: setBindingMock('ListAttachments', async () => []),
  };
}

function expectSilent(bindings: ReturnType<typeof draftBindings>): void {
  expect(bindings.save).not.toHaveBeenCalled();
  expect(bindings.get).not.toHaveBeenCalled();
  expect(bindings.clear).not.toHaveBeenCalled();
  expect(bindings.list).not.toHaveBeenCalled();
}

/** Settle the debounce window a persisting store would have used. */
async function settle(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 10));
}

describe('composerDraft store — persistence: none', () => {
  let bindings: ReturnType<typeof draftBindings>;

  beforeEach(() => {
    resetComposerDraftSnapshotsForTest();
    bindings = draftBindings();
  });

  it('seeds thread + content without hydrating', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'original message' }));

    expect(store.threadId).toBe('thread-1');
    expect(store.content).toBe('original message');
    expect(store.hydrating).toBe(false);
    expect(store.hasPendingSave).toBe(false);
    await settle();
    expectSilent(bindings);
  });

  it('refuses to seed a persisting store', async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    expect(() => store.seedLocalSnapshot('thread-1', snapshot())).toThrow(/persistence/);
  });

  it('exposes persists so callers can tell the two apart', () => {
    expect(localStore().persists).toBe(false);
    expect(createComposerDraftStore({ debounceMs: 0 }).persists).toBe(true);
  });

  it('mutates content, attachments and chips without saving or queueing', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: '' }));

    store.setContent('typed');
    store.addAttachment(sampleAttachment('att-1'));
    store.addTerminalChip({ id: 'chip-1', label: 'sh', preview: '$ ls', content: '$ ls', createdAt: 0 });

    expect(store.content).toBe('typed');
    expect(store.attachments.map((a) => a.id)).toEqual(['att-1']);
    expect(store.terminalChips.map((c) => c.id)).toEqual(['chip-1']);
    expect(store.hasDraft).toBe(true);
    expect(store.hasPendingSave).toBe(false);

    await settle();
    expectSilent(bindings);
  });

  it('keeps its edits out of the shared snapshot registry', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: '' }));
    store.setContent('editor-local text');
    store.setContentAndAttachments('editor-local text [Image #1]', [sampleAttachment('att-1')]);
    await settle();

    expect(getRememberedDraftSnapshot('thread-1')).toBeUndefined();
    expectSilent(bindings);
  });

  it('never clobbers the composer draft a persisting store holds for the same thread', async () => {
    const composerDraft = createComposerDraftStore({ debounceMs: 10_000 });
    await composerDraft.setThread('thread-1');
    composerDraft.setContent('what the user is typing');

    const editor = localStore();
    editor.seedLocalSnapshot('thread-1', snapshot({ content: 'a message being edited' }));
    editor.setContent('an edited message');
    editor.clearAfterSend();
    await settle();

    expect(composerDraft.content).toBe('what the user is typing');
    expect(getRememberedDraftSnapshot('thread-1')?.content).toBe('what the user is typing');
    expect(bindings.clear).not.toHaveBeenCalled();
  });

  it('composes an outgoing message from local state', () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({
      content: 'look at this: [Image #1]',
      attachments: [sampleAttachment('att-1')],
      terminalChips: [{ id: 'chip-1', label: 'sh', preview: '$ ls', content: '$ ls\nREADME', createdAt: 0 }],
    }));

    const outgoing = store.composeOutgoingMessage();
    expect(outgoing).toContain('look at this: [Image #1]');
    expect(outgoing).toContain('```terminal sh');
    expect(outgoing).toContain('README');
  });

  // ---- call sequences ----
  //
  // The mode is fixed at construction, so the risk is not a flag flip but a
  // lifecycle call arriving on a store that has no backend behind it.

  it('seed -> mutate -> clearAfterSend -> seed again', async () => {
    const store = localStore();

    store.seedLocalSnapshot('thread-1', snapshot({ content: 'first message' }));
    store.setContent('first message, edited');
    store.addAttachment(sampleAttachment('att-1'));

    store.clearAfterSend();
    expect(store.content).toBe('');
    expect(store.attachments).toEqual([]);
    expect(store.terminalChips).toEqual([]);
    expect(store.sourceProposedPlan).toBeNull();
    expect(store.hasPendingSave).toBe(false);
    expect(store.threadId).toBe('thread-1');

    store.seedLocalSnapshot('thread-2', snapshot({ content: 'second message' }));
    expect(store.threadId).toBe('thread-2');
    expect(store.content).toBe('second message');
    expect(store.hasPendingSave).toBe(false);

    store.setContent('second message, edited');
    await settle();
    expect(store.content).toBe('second message, edited');
    expectSilent(bindings);
    expect(getRememberedDraftSnapshot('thread-1')).toBeUndefined();
    expect(getRememberedDraftSnapshot('thread-2')).toBeUndefined();
  });

  it('setThread resets to an empty draft without hydrating', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'edited message' }));

    await store.setThread('thread-2');
    expect(store.threadId).toBe('thread-2');
    expect(store.content).toBe('');
    expect(store.hydrating).toBe(false);
    expect(store.hasPendingSave).toBe(false);

    await store.setThread(null);
    expect(store.threadId).toBeNull();
    expect(store.content).toBe('');

    await settle();
    expectSilent(bindings);
  });

  it('setThread on a dirty local store drops the edits instead of saving them', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'edited message' }));
    store.setContent('more edits');

    await store.setThread('thread-2');

    expect(store.content).toBe('');
    await settle();
    expectSilent(bindings);
    expect(getRememberedDraftSnapshot('thread-1')).toBeUndefined();
  });

  it('adoptThread moves the id and keeps the text, but arms no save', async () => {
    const store = localStore();
    store.seedLocalSnapshot(null, snapshot({ content: 'typed before materialization' }));

    store.adoptThread('thread-new');

    expect(store.threadId).toBe('thread-new');
    expect(store.content).toBe('typed before materialization');
    expect(store.hasPendingSave).toBe(false);
    await settle();
    expectSilent(bindings);
    expect(getRememberedDraftSnapshot('thread-new')).toBeUndefined();
  });

  it('reloadFromBackend leaves local state alone', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'edited message' }));

    await store.reloadFromBackend('thread-1');
    await store.reloadFromBackend();

    expect(store.content).toBe('edited message');
    expectSilent(bindings);
  });

  it('prepareForExternalDraftReplace resolves without touching anything', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'edited message' }));

    await store.prepareForExternalDraftReplace('thread-1');

    expect(store.content).toBe('edited message');
    expectSilent(bindings);
  });

  it('flush and flushPending are inert', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'edited message' }));
    store.setContent('unflushed');

    await store.flush();
    await store.flushPending();

    expect(store.content).toBe('unflushed');
    expect(store.hasPendingSave).toBe(false);
    expectSilent(bindings);
  });

  it('clearLocalAfterQueue clears local state only', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'queued message' }));

    store.clearLocalAfterQueue();

    expect(store.content).toBe('');
    expect(store.hasPendingSave).toBe(false);
    expectSilent(bindings);
  });

  it('restoreDraftFor repaints the local state for the current thread', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: '' }));

    await store.restoreDraftFor('thread-1', {
      content: 'restored message',
      attachments: [sampleAttachment('att-1')],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src', itemId: 'plan-1' },
    });

    expect(store.content).toBe('restored message');
    expect(store.attachments.map((a) => a.id)).toEqual(['att-1']);
    expect(store.sourceProposedPlan).toEqual({ threadId: 'src', itemId: 'plan-1' });
    expectSilent(bindings);
  });

  it('restoreDraftFor for another thread writes nothing anywhere', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: 'mine' }));

    await store.restoreDraftFor('thread-other', {
      content: 'not mine',
      attachments: [],
      terminalChips: [],
    });

    expect(store.content).toBe('mine');
    expectSilent(bindings);
    expect(getRememberedDraftSnapshot('thread-other')).toBeUndefined();
  });

  it('keeps the optimistic-restore markers working on local state', async () => {
    const store = localStore();
    store.seedLocalSnapshot('thread-1', snapshot({ content: '' }));
    const restored = snapshot({ content: 'interrupted prompt' });

    store.applyOptimisticRestoredDraft('thread-1', restored);
    expect(store.content).toBe('interrupted prompt');

    // Untouched restore clears back to the empty baseline.
    expect(store.clearOptimisticRestoredDraft('thread-1', restored)).toBe(true);
    expect(store.content).toBe('');

    // Edited restore is preserved.
    store.applyOptimisticRestoredDraft('thread-1', restored);
    store.setContent('edited prompt');
    expect(store.clearOptimisticRestoredDraft('thread-1', restored)).toBe(false);
    expect(store.content).toBe('edited prompt');

    await settle();
    expectSilent(bindings);
  });

});
