import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dispatchSend } from './composerSend';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import type { SourceProposedPlan, Thread } from '../../types/models';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setNewBranchName,
} from '../../stores/worktreeIntent.svelte';

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
      currentThread: thread,
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

  it('reports branch materialization failures with a normalized send error', async () => {
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const thread = { id: 'thread-1', branch: 'main' } as Thread;
    enterCreateBranchMode(thread, { workspaceDirty: true, currentBranch: 'main' });
    setNewBranchName(thread, 'BLITZ-187');
    const createBranch = setBindingMock('GitCreateBranchFrom', async () => {
      throw new Error('send message: create branch: branch "BLITZ-187" already exists');
    });
    const send = setBindingMock('SendMessageWithOptions', async () => thread);
    const restoreDraft = vi.fn(async () => {});
    const reportError = vi.fn();

    const sent = await dispatchSend({
      threadId: 'thread-1',
      message: 'continue',
      attachmentIds: [],
      snapshot: { content: 'continue', attachments: [], terminalChips: [] },
      currentThread: thread,
      restoreDraft,
      draftThreadId: () => 'thread-1',
      reportError,
    });

    expect(sent).toBe(false);
    expect(createBranch).toHaveBeenCalledWith('thread-1', 'BLITZ-187', 'main', true);
    expect(send).not.toHaveBeenCalled();
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
