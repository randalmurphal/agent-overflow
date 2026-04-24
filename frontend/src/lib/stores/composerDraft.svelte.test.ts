import { describe, expect, it, beforeEach } from 'vitest';
import { createComposerDraftStore } from './composerDraft.svelte';
import type { Attachment } from '../types/attachment';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

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

  it('switching threads discards pending saves from the previous thread', async () => {
    const store = createComposerDraftStore({ debounceMs: 50 });
    await store.setThread('thread-A');
    store.setContent('A-typed');
    // Before the debounce fires, switch threads.
    await store.setThread('thread-B');
    await new Promise((r) => setTimeout(r, 80));
    const saveMock = getBindingMock('SaveDraft');
    if (saveMock) {
      for (const call of saveMock.mock.calls) {
        expect(call[0]).not.toBe('thread-A');
      }
    }
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
});
