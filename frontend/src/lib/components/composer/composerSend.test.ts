import { describe, expect, it, vi } from 'vitest';
import { dispatchSend } from './composerSend';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import type { SourceProposedPlan, Thread } from '../../types/models';

describe('dispatchSend', () => {
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
      currentThread: thread,
      replaceCurrentThread: vi.fn(),
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
});
