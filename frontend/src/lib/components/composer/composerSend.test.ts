import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dispatchSend } from './composerSend';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import type { SourceProposedPlan, Thread } from '../../types/models';
import { resetForTest as resetWorktreeIntent } from '../../stores/worktreeIntent.svelte';

describe('dispatchSend', () => {
  beforeEach(() => {
    resetWorktreeIntent();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('passes revision source plan metadata when refining a proposed plan', async () => {
    const thread = { id: 'thread-1' } as Thread;
    const source: SourceProposedPlan = {
      threadId: 'thread-1',
      itemId: 'plan-1',
      payloadId: 'payload-1',
      title: 'Plan',
    };
    setBindingMock('SendMessageWithOptions', async () => thread);

    await dispatchSend({
      threadId: 'thread-1',
      message: 'Tighten the migration step.',
      attachmentIds: [],
      revisionSourceProposedPlan: source,
      snapshot: { content: 'Tighten the migration step.', attachments: [], terminalChips: [] },
      restoreDraft: vi.fn(),
      draftThreadId: () => 'thread-1',
      reportError: vi.fn(),
    });

    expect(getBindingMock('SendMessageWithOptions')).toHaveBeenCalledWith(
      'thread-1',
      'Tighten the migration step.',
      expect.objectContaining({
        revisionSourceProposedPlan: source,
      }),
    );
  });

  it('restores the draft and reports a normalized error when the send is refused', async () => {
    // The staged branch/worktree intent is applied by the composer BEFORE it
    // materializes and dispatches (Composer.send), so this layer only owns
    // the send itself and its rollback.
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const send = setBindingMock('SendMessageWithOptions', async () => {
      throw new Error('send message: create branch: branch "BLITZ-187" already exists');
    });
    const restoreDraft = vi.fn(async () => {});
    const reportError = vi.fn();

    const sent = await dispatchSend({
      threadId: 'thread-1',
      message: 'continue',
      attachmentIds: [],
      snapshot: { content: 'continue', attachments: [], terminalChips: [] },
      restoreDraft,
      draftThreadId: () => 'thread-1',
      reportError,
    });

    expect(sent).toBe(false);
    expect(send).toHaveBeenCalled();
    expect(restoreDraft).toHaveBeenCalledWith('thread-1', {
      content: 'continue',
      attachments: [],
      terminalChips: [],
    });
    expect(reportError).toHaveBeenCalledWith(
      'Failed to send message: Branch "BLITZ-187" already exists.',
    );
    expect(consoleErr).toHaveBeenCalled();
  });
});
